package agentplugin

import (
	"fmt"
	"os"
	"path/filepath"

	"wen/internal/i18n"
)

// SkillsDirName 与 MCPFile 是标准规定的固定位置。它们不可配置，也不接受清单里的
// 改写——「固定位置」正是这份标准让不同客户端读同一个包的办法。
const (
	SkillsDirName = "skills"
	MCPFile       = "mcp.json"
)

// Bundle 是一个已加载的插件包。
//
// Problems 是给人看的（CLI 与设置页），不进模型上下文：模型对「哪个文件写坏了」
// 无能为力，那是用户要修的东西。
type Bundle struct {
	Manifest Manifest
	// Format 是这个包的格式显示名（Agent Plugins / Claude / …），供界面与命令行说明。
	Format string
	Root   Root
	// SkillsDir 是包内的技能根目录，不存在时为空串。
	// 里面每个子目录一个技能，具体解析交给技能插件，本包不重复实现一遍。
	SkillsDir string
	// HasMCP 表示包里有 mcp.json。整份文件不合法时它仍为 true 而 Servers 为空：
	// 「有这个组件但没能用起来」与「压根没这个组件」是两件事，报给人看的话不一样。
	HasMCP bool
	// Servers 是 mcp.json 里可用的那些 server，尚未展开占位符。
	Servers  []MCPServer
	Problems []string
}

// Load 读取一个插件包目录。
//
// 失败的边界按标准划：清单读不出来或不合法 → 整个包不可用（返回 error）；
// 某个组件缺失或有问题 → 记一条 problem 继续走。缺 skills/ 与缺 mcp.json 都不是
// 问题，只是这个包没有那种组件。
func Load(lang, dir string) (*Bundle, error) {
	root, err := NewRoot(lang, dir)
	if err != nil {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.open_dir", "打不开插件包目录：%w"), err)
	}

	f, manifestPath, err := detect(lang, root)
	if err != nil {
		return nil, err
	}
	if _, err := root.Contains(manifestPath); err != nil {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.manifest_unusable", "清单不可用：%w"), err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.read_manifest", "读取清单失败：%w"), err)
	}
	var m Manifest
	var warnings []string
	if f.Strict {
		m, warnings, err = ParseManifest(lang, data)
	} else {
		m, warnings, err = parseLoose(lang, data, f.Name)
	}
	if err != nil {
		return nil, err
	}

	b := &Bundle{Manifest: m, Format: f.Name, Root: root, Problems: warnings}

	// 技能组件：目录在就收下，里面每一篇的合规性由技能插件在扫描时判定。
	skillsDir := root.Join(SkillsDirName)
	if info, err := os.Stat(skillsDir); err == nil && info.IsDir() {
		if real, err := root.Contains(skillsDir); err != nil {
			b.Problems = append(b.Problems, i18n.Tf(lang, "plugin.bundle.warn.skills_escape",
				"%s/ 指向插件包之外，已忽略整个技能组件", SkillsDirName))
		} else {
			b.SkillsDir = real
		}
	}

	// MCP 组件：整份文件坏掉只作废这一个组件，技能照常；单台 server 坏掉只跳过那一台。
	if mcpPath := findMCP(root); mcpPath != "" {
		b.HasMCP = true
		switch data, err := os.ReadFile(mcpPath); {
		case err != nil:
			b.Problems = append(b.Problems, i18n.Tf(lang, "plugin.bundle.warn.read_mcp",
				"读取 %s 失败：%s", filepath.Base(mcpPath), err.Error()))
		default:
			var servers []MCPServer
			var warnings []string
			if f.Strict {
				servers, warnings, err = ParseMCP(lang, data)
			} else {
				servers, warnings, err = ParseMCPLoose(lang, data)
			}
			b.Problems = append(b.Problems, warnings...)
			if err != nil {
				b.Problems = append(b.Problems, i18n.Tf(lang, "plugin.bundle.warn.mcp_unusable",
					"%s，这个包的 MCP 组件整个不可用", err.Error()))
			} else {
				b.Servers = servers
			}
		}
	}
	return b, nil
}
