package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"

	"wen/internal/agentplugin"
	"wen/internal/config"
	"wen/internal/runlock"
)

// runPlugins 管理外源插件包（Agent Plugins 1.0）。
//
// 装与卸都只是动文件，因此不区分在线/离线——但服务正在跑的时候，插件是在启动期
// 一次性注册的，改动要重启才生效。这一点每次都说一句，别让人装完之后对着没有变化
// 的界面猜。运行时增删插件不是不能做，但那要动注册顺序、全局工具重名检查与
// Manager 的锁，为一次性的安装动作不值得（openclaw 那边同样要求重启）。
func runPlugins(args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("plugins "+sub, flag.ExitOnError)
	configPath := fs.String("c", "", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）")
	rest := parseMixed(fs, args)

	path := config.ResolvePath(*configPath)
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	store := agentplugin.NewStore(cfg.BaseDir)
	if store.Dir() == "" {
		return fmt.Errorf("没有可用的配置目录，无法管理插件包")
	}

	switch sub {
	case "list":
		return listBundles(store)
	case "install":
		if len(rest) != 1 {
			return fmt.Errorf("用法: wen plugins install <目录或 .zip>")
		}
		return installBundle(store, rest[0], cfg)
	case "uninstall", "remove":
		if len(rest) != 1 {
			return fmt.Errorf("用法: wen plugins uninstall <插件包名称>")
		}
		return uninstallBundle(store, rest[0], cfg.BaseDir)
	case "info":
		if len(rest) != 1 {
			return fmt.Errorf("用法: wen plugins info <插件包名称>")
		}
		return bundleInfo(store, rest[0])
	default:
		return fmt.Errorf("未知的子命令 %q（可用：list / install / uninstall / info）", sub)
	}
}

// parseMixed 解析可以与位置参数混着写的选项，返回位置参数。
//
// flag 包遇到第一个非选项就停下，于是 wen plugins install <目录> -c <配置> 里的 -c
// 会被当成位置参数——而「路径写在前面、选项写在后面」恰恰是最顺手的写法。
func parseMixed(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return positional
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func listBundles(store *agentplugin.Store) error {
	bundles, problems := store.List()
	fmt.Printf("插件包目录  %s\n\n", store.Dir())
	if len(bundles) == 0 && len(problems) == 0 {
		fmt.Println("还没有装过插件包。用 wen plugins install <目录或 .zip> 装一个。")
		return nil
	}
	if len(bundles) > 0 {
		// 用显示宽度对齐而不是 tabwriter：内容那一列是中文，而 tabwriter 按字节数
		// 算宽度，中日韩字符一进去整张表就歪了。
		nameW, verW, kindW := 4, 4, 4
		kinds := make([]string, len(bundles))
		for i, b := range bundles {
			kinds[i] = strings.Join(components(b), "+")
			nameW = max(nameW, runewidth.StringWidth(b.Manifest.Name))
			verW = max(verW, runewidth.StringWidth(dash(b.Manifest.Version)))
			kindW = max(kindW, runewidth.StringWidth(kinds[i]))
		}
		fmt.Printf("%s  %s  %s  %s\n",
			pad("名称", nameW), pad("版本", verW), pad("内容", kindW), "说明")
		for i, b := range bundles {
			fmt.Printf("%s  %s  %s  %s\n",
				pad(b.Manifest.Name, nameW), pad(dash(b.Manifest.Version), verW),
				pad(kinds[i], kindW), firstLine(b.Manifest.Description))
		}
	}
	for _, p := range problems {
		fmt.Printf("\n× %s\n", p)
	}
	return nil
}

func installBundle(store *agentplugin.Store, src string, cfg *config.Config) error {
	b, err := store.Install(src)
	if err != nil {
		return err
	}
	fmt.Printf("已装好 %s%s\n", b.Manifest.Name, paren(b.Manifest.Version))
	fmt.Printf("  位置  %s\n", store.Path(b.Manifest.Name))
	fmt.Printf("  内容  %s\n", strings.Join(components(b), "、"))
	fmt.Printf("  插件  %s（设置页上标记为「外源」，可单独开关）\n", agentplugin.PluginName(b.Manifest.Name))
	for _, p := range b.Problems {
		fmt.Printf("  提示  %s\n", p)
	}
	if b.SkillsDir != "" {
		noteSkillsOff(cfg)
	}
	if b.HasMCP {
		fmt.Println("\n注意：这个包里的 MCP server 声明本版还不会加载，只有技能会生效。")
	}
	noteRestart(cfg.BaseDir)
	return nil
}

func uninstallBundle(store *agentplugin.Store, name, baseDir string) error {
	if err := store.Remove(name); err != nil {
		return err
	}
	fmt.Printf("已卸载 %s\n", name)
	fmt.Printf("提示：它在设置页上的开关状态还留在 plugins.state.json 里，重新装回来时会沿用。\n")
	noteRestart(baseDir)
	return nil
}

func bundleInfo(store *agentplugin.Store, name string) error {
	b, err := agentplugin.Load(store.Path(name))
	if err != nil {
		return fmt.Errorf("%s：%w", name, err)
	}
	m := b.Manifest
	fmt.Printf("名称      %s\n", m.Name)
	fmt.Printf("插件名    %s\n", agentplugin.PluginName(m.Name))
	if m.Version != "" {
		fmt.Printf("版本      %s\n", m.Version)
	}
	if m.Description != "" {
		fmt.Printf("说明      %s\n", m.Description)
	}
	if m.Author != nil && m.Author.Name != "" {
		fmt.Printf("作者      %s\n", m.Author.Name)
	}
	if m.License != "" {
		fmt.Printf("许可      %s\n", m.License)
	}
	if m.Homepage != "" {
		fmt.Printf("主页      %s\n", m.Homepage)
	}
	fmt.Printf("位置      %s\n", b.Root.Path())
	fmt.Printf("内容      %s\n", strings.Join(components(b), "、"))
	if b.SkillsDir != "" {
		fmt.Printf("技能目录  %s\n", b.SkillsDir)
	}
	for _, p := range b.Problems {
		fmt.Printf("提示      %s\n", p)
	}
	return nil
}

// components 说清这个包里到底有什么。空包也要说，否则「装好了但什么都没发生」
// 只能靠猜。
func components(b *agentplugin.Bundle) []string {
	var out []string
	if b.SkillsDir != "" {
		out = append(out, "技能")
	}
	if b.HasMCP {
		out = append(out, "MCP")
	}
	if len(out) == 0 {
		out = append(out, "（空）")
	}
	return out
}

// noteRestart 在服务正在运行时提醒重启。
// noteSkillsOff 在「技能手册」插件没开着时说一句。
//
// 它默认是关的（技能目录空着时开着只会多两个用不上的工具），于是装了个带技能的包
// 会表现为「装好了，但什么都没发生」——这是最难自己查出来的一类问题，装的时候就该
// 说清楚。查不出状态时不猜也不吓唬人，直接不说。
func noteSkillsOff(cfg *config.Config) {
	b, err := openBackend(cfg)
	if err != nil {
		return
	}
	list, err := b.listPlugins()
	if err != nil {
		return
	}
	for _, st := range list {
		if st.Name == "skills" && !st.Enabled {
			fmt.Println()
			fmt.Println("注意：「技能手册」插件当前是关的，包里的技能要等它打开之后才进清单。")
			fmt.Println("      用 wen config plugins 打开它，或在设置页的「基础工具」一节里打开。")
			return
		}
	}
}

func noteRestart(baseDir string) {
	if info, ok := runlock.Read(baseDir); ok && reachable(info.Addr) {
		fmt.Printf("\n服务正在 %s 运行，插件在启动期注册，重启之后才会生效。\n", info.Addr)
	}
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func paren(s string) string {
	if s == "" {
		return ""
	}
	return "（" + s + "）"
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}
