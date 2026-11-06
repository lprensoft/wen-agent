package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wen/internal/backup"
	"wen/internal/config"
	"wen/internal/plugin"
	"wen/internal/runlock"
	"wen/internal/version"
)

// runExport 把配置目录里程序维护的数据打成归档。
func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	configPath := fs.String("c", "", tr("cli.flag.config", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）"))
	out := fs.String("o", "", tr("cli.export.flag.output", "备份文件路径（默认 wen-backup-<时间>.tar.gz）"))
	force := fs.Bool("force", false, tr("cli.export.flag.force", "覆盖已有文件，并在服务运行时继续（可能不一致）"))
	rest, err := parseBackupFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("%s", tr("cli.export.usage", "用法：wen export [-o 备份文件] [-c 配置文件] [--force]"))
	}

	path := config.ResolvePath(*configPath)
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf(tr("cli.err.load_config", "加载配置失败: %w"), err)
	}
	adoptLang(cfg)

	if addr, ok := serviceRunning(cfg.BaseDir); ok && !*force {
		return fmt.Errorf("%s", trf("cli.export.err.running", "服务正在运行（%s）；先停止服务再备份，或加 --force 继续（快照可能不一致）", addr))
	}

	output := *out
	if output == "" {
		output = "wen-backup-" + time.Now().Format("20060102-150405") + ".tar.gz"
	}

	sessionDir := cfg.SessionDir()
	if rel, ok := pathWithin(cfg.BaseDir, sessionDir); !ok || rel == "." {
		fmt.Print(trf("cli.export.warn_external_sessions", "提示：会话目录 %s 不在配置目录内或就是配置目录本身，本次备份不单独包含它。\n", sessionDir))
	}
	var extra []string
	for _, dir := range projectMemoryDirs(cfg) {
		if rel, ok := pathWithin(cfg.BaseDir, dir); ok && rel != "." {
			extra = append(extra, dir)
		} else {
			fmt.Print(trf("cli.export.warn_project_memory", "提示：项目记忆在 %s，本次备份不包含它。\n", dir))
		}
	}

	manifest, stats, err := backup.Export(cfg.BaseDir, output, backup.Options{
		Lang:       uiLang,
		AppVersion: version.Version,
		ConfigFile: filepath.Base(path),
		SessionDir: sessionDir,
		ExtraDirs:  extra,
		Overwrite:  *force,
	})
	if err != nil {
		return err
	}

	fmt.Print(trf("cli.export.done", "已备份 %d 个文件（%d 字节）到 %s\n", manifest.FileCount, manifest.TotalBytes, output))
	if stats.SkippedSymlinks > 0 {
		fmt.Print(trf("cli.export.symlinks", "跳过了 %d 个符号链接。\n", stats.SkippedSymlinks))
	}
	fmt.Print(tr("cli.export.secrets", "注意：备份包含模型密钥、消息通道凭据、访问口令派生值与全部会话内容，请妥善保管。\n"))
	return nil
}

// runImport 把一个备份恢复到一个空目录。
func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	configPath := fs.String("c", "", tr("cli.flag.config", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）"))
	rest, err := parseBackupFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("%s", tr("cli.import.usage", "用法：wen import <备份文件> [-c 配置文件]"))
	}

	path := config.ResolvePath(*configPath)
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf(tr("cli.err.load_config", "加载配置失败: %w"), err)
	}
	adoptLang(cfg)

	if addr, ok := serviceRunning(cfg.BaseDir); ok {
		return fmt.Errorf("%s", trf("cli.import.err.running", "服务正在运行（%s）；先停止服务再恢复。", addr))
	}

	manifest, stats, err := backup.Import(rest[0], cfg.BaseDir, uiLang)
	if err != nil {
		return err
	}

	fmt.Print(trf("cli.import.done", "已恢复 %d 个文件（%d 字节）到 %s\n", stats.Files, stats.Bytes, cfg.BaseDir))
	if manifest.AppVersion != "" && manifest.AppVersion != version.Version {
		fmt.Print(trf("cli.import.version_mismatch", "提示：备份来自 %s，当前程序是 %s。\n", manifest.AppVersion, version.Version))
	}
	configFile := "config.yaml"
	if manifest.ConfigFile != "" && filepath.Base(manifest.ConfigFile) == manifest.ConfigFile {
		configFile = manifest.ConfigFile
	}
	fmt.Print(trf("cli.import.next", "下一步：wen status -c %s\n", filepath.Join(cfg.BaseDir, configFile)))
	return nil
}

// parseBackupFlags 允许位置参数与选项交错（wen import a.tar.gz -c x.yaml），
// 与 parseMixed 相同；区别是它把 flag 的解析错误交回调用方，不静默忽略。
func parseBackupFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// serviceRunning 判断配置目录下是否有一个真正在响应的实例。
func serviceRunning(dir string) (string, bool) {
	info, ok := runlock.Read(dir)
	if !ok || !reachable(info.Addr) {
		return "", false
	}
	return info.Addr, true
}

// pathWithin 返回 p 相对 base 的路径，以及 p 是否落在 base 之内。
func pathWithin(base, p string) (string, bool) {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", false
	}
	absP, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(absBase, absP)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// projectMemoryDirs 在 memory 插件使用按项目分库时返回全部记忆库目录。
// 带可见域的记忆存放在 base 同级的 <base>-<tag>，因此不能只取 base。
// 它只读插件状态文件；读不出来就当没有，不让备份因为一条提示而失败。
func projectMemoryDirs(cfg *config.Config) []string {
	raw, err := os.ReadFile(filepath.Join(cfg.BaseDir, "plugins.state.json"))
	if err != nil {
		return nil
	}
	var state map[string]struct {
		Config map[string]any `json:"config"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return nil
	}
	mem, ok := state["memory"]
	if !ok || mem.Config == nil {
		return nil
	}
	if scope, _ := mem.Config["scope"].(string); scope != "project" {
		return nil
	}
	workdir := cfg.Agent.Workdir
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	base := filepath.Join(workdir, ".wen", "memories")
	var out []string
	for _, tag := range plugin.ReadDomains(base, plugin.Scope{}) {
		dir := plugin.DomainDir(base, tag)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			out = append(out, dir)
		}
	}
	return out
}
