package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

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
	noConnect := fs.Bool("no-connect", false, "装完不连接包里的 MCP server（不取工具清单）")
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
		return installBundle(store, rest[0], cfg, *noConnect)
	case "uninstall", "remove":
		if len(rest) != 1 {
			return fmt.Errorf("用法: wen plugins uninstall <插件包名称>")
		}
		return uninstallBundle(store, rest[0], cfg.BaseDir)
	case "refresh":
		if len(rest) != 1 {
			return fmt.Errorf("用法: wen plugins refresh <插件包名称>")
		}
		return refreshBundle(store, rest[0], cfg)
	case "info":
		if len(rest) != 1 {
			return fmt.Errorf("用法: wen plugins info <插件包名称>")
		}
		return bundleInfo(store, rest[0])
	default:
		return fmt.Errorf("未知的子命令 %q（可用：list / install / refresh / uninstall / info）", sub)
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
			tools, _ := store.LoadTools(b.Manifest.Name)
			kinds[i] = strings.Join(components(b, len(tools)), "+")
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

func installBundle(store *agentplugin.Store, src string, cfg *config.Config, noConnect bool) error {
	b, err := store.Install(src)
	if err != nil {
		return err
	}
	fmt.Printf("已装好 %s%s\n", b.Manifest.Name, paren(b.Manifest.Version))
	fmt.Printf("  位置  %s\n", store.Path(b.Manifest.Name))
	fmt.Printf("  内容  %s\n", strings.Join(components(b, -1), "、"))
	fmt.Printf("  插件  %s（设置页上标记为「外源」，可单独开关）\n", agentplugin.PluginName(b.Manifest.Name))
	for _, p := range b.Problems {
		fmt.Printf("  提示  %s\n", p)
	}
	if b.SkillsDir != "" {
		noteSkillsOff(cfg)
	}
	if len(b.Servers) > 0 && !noConnect {
		if err := handshake(store, b, cfg); err != nil {
			// 握手失败不回滚安装：包已经装好了，连不上多半是那台 server 自己的事
			// （依赖没装、要先登录），用 wen plugins refresh 重来一次即可。
			fmt.Println("  × 取工具清单失败：" + err.Error())
			fmt.Println("    包已装好，修好之后用 wen plugins refresh " + b.Manifest.Name + " 重试。")
		}
	} else if b.HasMCP && len(b.Servers) == 0 {
		fmt.Println()
		fmt.Println("注意：这个包里有 mcp.json，但没有一台可用的 server（原因见上面的提示）。")
	} else if noConnect && len(b.Servers) > 0 {
		fmt.Println()
		fmt.Println("按要求没有连接。包里的工具要等 wen plugins refresh " + b.Manifest.Name + " 之后才可用。")
	}
	noteRestart(cfg.BaseDir)
	return nil
}

// refreshBundle 连一遍包里的 MCP server，把工具清单重取一遍。
func refreshBundle(store *agentplugin.Store, name string, cfg *config.Config) error {
	b, err := agentplugin.Load(store.Path(name))
	if err != nil {
		return fmt.Errorf("%s：%w", name, err)
	}
	return handshake(store, b, cfg)
}

// handshake 取回工具清单并落盘，把过程如实说清楚。
//
// 它会**在本机启动插件包里声明的命令**。这一步不藏着：装一个包本来就意味着以后要跑
// 它，但「以后」和「现在」是两回事，所以把将要执行的命令原样打出来。不想连的话
// install 时加 --no-connect，之后再用 wen plugins refresh。
func handshake(store *agentplugin.Store, b *agentplugin.Bundle, cfg *config.Config) error {
	if len(b.Servers) == 0 {
		return nil
	}
	// 把将要执行的命令原样打出来。只列 stdio 的那些——HTTP 的 server 不在本机启动，
	// 混在同一张「将在本机执行」的清单里是句错话。
	var local []agentplugin.MCPServer
	for _, s := range b.Servers {
		if s.Type == agentplugin.TransportStdio {
			local = append(local, s)
		}
	}
	fmt.Println()
	fmt.Println(fmt.Sprint("正在连接 ", len(b.Servers), " 台 MCP server。其中 ", len(local), " 台将在本机启动："))
	for _, s := range local {
		fmt.Println("  " + s.Name + "  " + strings.TrimSpace(s.Command+" "+strings.Join(s.Args, " ")))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tools, problems, err := agentplugin.Handshake(ctx, b, agentplugin.DataDir(cfg.BaseDir, b.Manifest.Name))
	if err != nil {
		return err
	}
	if err := store.SaveTools(b.Manifest.Name, tools); err != nil {
		return fmt.Errorf("工具清单没能存下来：%w", err)
	}

	fmt.Println(fmt.Sprint("取到 ", len(tools), " 件工具"))
	for _, t := range tools {
		fmt.Println("  " + t.Local)
	}
	for _, m := range problems {
		fmt.Println("  × " + m)
	}
	return nil
}

func uninstallBundle(store *agentplugin.Store, name, baseDir string) error {
	if err := store.Remove(name); err != nil {
		return err
	}
	store.DropTools(name)
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
	fmt.Println("格式      " + b.Format)
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
	tools, _ := store.LoadTools(name)
	fmt.Printf("内容      %s\n", strings.Join(components(b, len(tools)), "、"))
	if b.SkillsDir != "" {
		fmt.Printf("技能目录  %s\n", b.SkillsDir)
	}
	for _, p := range b.Problems {
		fmt.Printf("提示      %s\n", p)
	}
	for _, t := range tools {
		fmt.Println("工具      " + t.Local)
	}
	if len(tools) == 0 && b.HasMCP {
		fmt.Println("提示      还没取到工具，用 wen plugins refresh " + name + " 连一次")
	}
	return nil
}

// components 说清这个包里到底有什么。空包也要说，否则「装好了但什么都没发生」
// 只能靠猜。
func components(b *agentplugin.Bundle, tools int) []string {
	var out []string
	if b.SkillsDir != "" {
		out = append(out, "技能")
	}
	if b.HasMCP {
		// 带上工具数：一个包「有 MCP 声明」与「真的取到了工具」是两回事，
		// 而后者才决定模型能不能用上它
		if tools < 0 {
			out = append(out, "MCP") // 还没握手，件数未知
		} else {
			out = append(out, fmt.Sprint("MCP·", tools, " 件工具"))
		}
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
