package agentplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// 除 Agent Plugins 之外，几家客户端各有自己的插件包布局。它们的差别只在清单放在哪儿
// ——内容的固定位置（skills/、mcp.json）是一样的，因为那两样本来就来自同一批规范。
//
// 认这些布局的理由是现实：这批格式比 Agent Plugins 出现得早，社区里已经有很多包。
// 不认的话，用户得先手工改一个 plugin.json 才能装。
type format struct {
	Name     string // 显示用
	Manifest string // 清单在包内的相对位置
	Strict   bool   // 是否按 Agent Plugins 的闭合 schema 校验
}

var formats = []format{
	{Name: "Agent Plugins", Manifest: ManifestFile, Strict: true},
	{Name: "Claude", Manifest: filepath.Join(".claude-plugin", "plugin.json")},
	{Name: "Codex", Manifest: filepath.Join(".codex-plugin", "plugin.json")},
	{Name: "Cursor", Manifest: filepath.Join(".cursor-plugin", "plugin.json")},
}

// mcpFiles 是 MCP 声明可能在的位置，按先后顺序找。
// 点开头的那个是几家客户端的写法，与仓库根目录下的 .mcp.json 是同一个东西。
var mcpFiles = []string{MCPFile, ".mcp.json"}

// detect 找出这个目录是哪一种插件包，返回清单路径与格式。
func detect(root Root) (format, string, error) {
	for _, f := range formats {
		p := root.Join(f.Manifest)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return f, p, nil
		}
	}
	return format{}, "", fmt.Errorf("这个目录里没有 %s，也不是 Claude / Codex / Cursor 的插件包", ManifestFile)
}

// parseLoose 读别家格式的清单。
//
// 只取本程序用得上的那几个字段，其余一概不管：那些格式各有各的扩展（hooks、agents、
// 命令），本程序不认识它们，也不该因为看不懂就拒绝整个包。名字仍按 Agent Plugins 的
// 规则校验——它要拼进目录名与插件名，这条不能松。
func parseLoose(data []byte, formatName string) (Manifest, []string, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, nil, fmt.Errorf("%s 插件包的清单不是合法的 JSON：%w", formatName, err)
	}
	if err := checkName(m.Name); err != nil {
		return Manifest{}, nil, err
	}
	warnings := []string{fmt.Sprintf("这是一个 %s 格式的插件包，只读得懂里面的技能与 MCP 声明，"+
		"该格式特有的内容（命令、子代理、钩子等）不会生效", formatName)}
	return m, warnings, nil
}

// findMCP 返回包内 MCP 声明的位置，没有则返回空串。
func findMCP(root Root) string {
	for _, name := range mcpFiles {
		p := root.Join(name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}
