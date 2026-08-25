package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"wen/internal/config"
	"wen/internal/runlock"
	"wen/internal/updater"
	"wen/internal/version"
)

// runUpdate 是 wen update：命令行下的自更新。
//
// 与设置页上那个按钮走的是同一套 internal/updater，区别只在「谁来确认」——这里靠
// 显式的 -y，而界面上靠按钮文案从「检查更新」变成「更新到 vX 并重启」。它也不负责
// 重启：命令行更新的时候服务多半在另一个进程里跑着，从外面把它掀掉不是这个命令
// 该做的事，说清楚「重启后生效」就够了。
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	configPath := fs.String("c", "", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）")
	checkOnly := fs.Bool("check", false, "只检查有没有新版，不下载安装")
	yes := fs.Bool("y", false, "确认更新（不加它只报告，不动程序文件）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// 语言在最前面定下：这个命令的输出从第一行就开始了，配置读不出来也不影响
	// 它按系统区域猜一门
	if cfg, err := config.Load(config.ResolvePath(*configPath)); err == nil {
		adoptLang(cfg)
	} else {
		adoptLang(nil)
	}

	exe, err := updater.ExePath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client := updater.NewClient(version.Version)
	fmt.Printf("当前版本 %s，正在查询最新的正式版…\n", version.Version)
	rel, err := client.Latest(ctx)
	if err != nil {
		return err
	}

	if !updater.Newer(version.Version, rel.Tag) {
		if cur, ok := updater.ParseVersion(version.Version); ok && cur.Dev {
			fmt.Printf("当前是开发版 %s，比最新的正式版 %s 还新，无需更新。\n", version.Version, rel.Tag)
			return nil
		}
		fmt.Printf("已是最新版 %s。\n", version.Version)
		return nil
	}

	fmt.Printf("\n发现新版 %s", rel.Tag)
	if !rel.PublishedAt.IsZero() {
		fmt.Printf("（发布于 %s）", rel.PublishedAt.Local().Format("2006-01-02"))
	}
	fmt.Println("：")
	if notes := updater.ReleaseNotes(rel.Body); notes != "" {
		fmt.Println()
		fmt.Println(indent(notes, "  "))
	}
	if *checkOnly {
		return nil
	}
	if !*yes {
		fmt.Printf("\n确认无误后执行 wen update -y 开始更新（会替换 %s）。\n", exe)
		return nil
	}

	if err := updater.CheckWritable(exe); err != nil {
		return err
	}
	plan, err := client.Prepare(ctx, rel)
	if err != nil {
		return err
	}
	work := updater.WorkDir(exe)
	fmt.Printf("\n正在下载 %s…\n", plan.Asset.Name)
	archive, err := client.Fetch(ctx, plan, work, func(done, total int64) {
		if total > 0 {
			fmt.Fprintf(os.Stderr, "\r  %d%%（%s / %s）", done*100/total, humanSize(done), humanSize(total))
		}
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}

	fmt.Println("校验通过，正在解包…")
	bin, err := updater.Extract(archive, work, updater.HostBinaryName())
	if err != nil {
		return err
	}
	fmt.Println("正在试运行新版程序…")
	if err := updater.SmokeTest(ctx, bin, rel.Tag); err != nil {
		return err
	}
	if err := updater.Apply(bin, exe); err != nil {
		return err
	}
	fmt.Printf("已更新到 %s。\n", rel.Tag)

	// 服务正在跑的话，跑着的仍是旧版：文件换掉不会影响已经载入内存的那一份
	if cfg, err := config.Load(config.ResolvePath(*configPath)); err == nil {
		if info, ok := runlock.Read(cfg.BaseDir); ok && reachable(info.Addr) {
			fmt.Printf("注意：有一个服务实例正在 %s 运行，它跑的仍是 %s。重启它才会用上新版；\n"+
				"      也可以在设置页的「程序更新」里一键更新并重启。\n", info.Addr, info.Version)
			return nil
		}
	}
	fmt.Println("下次启动即为新版。")
	return nil
}

// humanSize 把字节数说成人话。
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d 字节", n)
	}
}

// indent 给多行文本整体加一段缩进，让更新说明在终端里与上下文分开。
func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + strings.TrimRight(l, "\r")
	}
	return strings.Join(lines, "\n")
}
