package main

import (
	"flag"
	"fmt"
	"strings"

	"wen/internal/config"
	"wen/internal/runlock"
	"wen/internal/version"
)

// runStatus 打印当前配置与运行状态。不进交互，便于 ssh 一条命令看清楚现状。
func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	configPath := fs.String("c", "", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）")
	_ = fs.Parse(args)

	path := config.ResolvePath(*configPath)
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	adoptLang(cfg)
	b, err := openBackend(cfg)
	if err != nil {
		return err
	}

	fmt.Printf("Wen Agent %s\n", version.Version)
	fmt.Printf("配置文件  %s\n", path)
	fmt.Printf("配置目录  %s\n", cfg.BaseDir)

	if info, ok := runlock.Read(cfg.BaseDir); ok && reachable(info.Addr) {
		fmt.Printf("运行状态  运行中 · 监听 %s · PID %d · 版本 %s\n", info.Addr, info.PID, info.Version)
	} else {
		fmt.Printf("运行状态  未运行（配置里的监听地址为 %s:%d）\n", cfg.Server.Host, cfg.Server.Port)
	}

	if auth, err := b.loadAuth(); err == nil {
		fmt.Printf("访问控制  %s\n", describeAuth(auth))
	}

	if doc, err := b.loadModels(); err == nil {
		fmt.Printf("当前模型  %s / %s\n", doc.Current.Provider, doc.Current.Model)
	}

	list, err := b.listPlugins()
	if err != nil {
		return err
	}
	var on, off []string
	for _, p := range list {
		if p.Enabled {
			on = append(on, p.Name)
		} else {
			off = append(off, p.Name)
		}
	}
	fmt.Printf("已启用插件（%d）%s\n", len(on), wrapNames(on))
	fmt.Printf("已禁用插件（%d）%s\n", len(off), wrapNames(off))
	fmt.Printf("\n数据来源  %s\n", b.mode())
	return nil
}

func wrapNames(names []string) string {
	if len(names) == 0 {
		return "  （无）"
	}
	return "\n  " + strings.Join(names, "、")
}

// describeAuth 与服务端启动日志的 authSummary 说的是同一件事，
// 措辞保持一致；此处多说一句「口令来自哪里」，因为 CLI 常在排查时用。
func describeAuth(a authState) string {
	switch {
	case a.EnvManaged:
		return tr("cli.auth.env", "已设置口令（来自环境变量 WEN_AUTH_PASSWORD）")
	case !a.HasPassword:
		return tr("cli.auth.none", "未设置口令，仅本机可访问")
	case !a.TrustLoop:
		return tr("cli.auth.allLogin", "已设置口令，所有来源（含本机）都需登录")
	case a.Exposed:
		return tr("cli.auth.loopFree", "已设置口令，本机免登录、其它来源需登录")
	default:
		return tr("cli.auth.localOnly", "已设置口令，但当前只监听本机，不会被外部访问")
	}
}
