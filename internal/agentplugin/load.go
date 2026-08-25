package agentplugin

import (
	"fmt"
	"os"
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
	Root     Root
	// SkillsDir 是包内的技能根目录，不存在时为空串。
	// 里面每个子目录一个技能，具体解析交给技能插件，本包不重复实现一遍。
	SkillsDir string
	// HasMCP 表示包里有 mcp.json。M1 只记录不加载——先把它报出来，
	// 用户装了个纯 MCP 的包却什么都没发生时，至少知道是本程序还没支持。
	HasMCP   bool
	Problems []string
}

// Load 读取一个插件包目录。
//
// 失败的边界按标准划：清单读不出来或不合法 → 整个包不可用（返回 error）；
// 某个组件缺失或有问题 → 记一条 problem 继续走。缺 skills/ 与缺 mcp.json 都不是
// 问题，只是这个包没有那种组件。
func Load(dir string) (*Bundle, error) {
	root, err := NewRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("打不开插件包目录：%w", err)
	}

	manifestPath := root.Join(ManifestFile)
	if _, err := root.Contains(manifestPath); err != nil {
		return nil, fmt.Errorf("%s 不可用：%w", ManifestFile, err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", ManifestFile, err)
	}
	m, warnings, err := ParseManifest(data)
	if err != nil {
		return nil, err
	}

	b := &Bundle{Manifest: m, Root: root, Problems: warnings}

	// 技能组件：目录在就收下，里面每一篇的合规性由技能插件在扫描时判定。
	skillsDir := root.Join(SkillsDirName)
	if info, err := os.Stat(skillsDir); err == nil && info.IsDir() {
		if real, err := root.Contains(skillsDir); err != nil {
			b.Problems = append(b.Problems, SkillsDirName+"/ 指向插件包之外，已忽略整个技能组件")
		} else {
			b.SkillsDir = real
		}
	}

	// MCP 组件：本版只报告存在与否。真正的加载在 M2。
	if info, err := os.Stat(root.Join(MCPFile)); err == nil && !info.IsDir() {
		b.HasMCP = true
	}
	return b, nil
}
