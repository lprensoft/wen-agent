// Package agentplugin 实现 Agent Plugins 1.0 标准的客户端：把一个插件包目录
// （plugin.json + skills/ + mcp.json）读进来，交给插件系统当「外源」插件使用。
//
// 标准本身不含任何可执行代码，只是「若干篇 markdown + 若干条 MCP server 声明」，
// 因此它与宿主用什么语言写无关。选它而不是自定义一份清单格式，是为了直接接上
// ChatGPT、Codex、Cursor、Copilot、VS Code 这些客户端共享的那批插件包——
// 判断依据见 docs/decisions/agent-plugins-analysis.md。
//
// 本包只做「读与校验」，不认识 wen 的任何概念；把它接进插件系统的是同目录的
// plugin.go。这条界线是有意的：标准的规则要能单独测，不必把插件系统建起来。
package agentplugin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"wen/internal/i18n"
)

// ManifestFile 是插件包根目录下那份必需的清单。
const ManifestFile = "plugin.json"

// SchemaURL 是本实现支持的清单 schema。标准要求客户端按 $schema 选择支持的版本，
// 不认识的版本要拒绝整个插件包（而不是当它没写过）。
const SchemaURL = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"

// Author 是清单里的作者信息。
type Author struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Manifest 是 plugin.json 的内容。
//
// 标准规定这是一份**闭合 schema**：顶层只允许这几个字段。但「闭合」不等于「见到
// 别的字段就报废」——未知字段要报告并忽略，插件包仍然有效。这条差别很实际：
// 标准会往后长，用 1.0 的客户端读 1.1 写的包时，多出来的字段不该让包装不上。
type Manifest struct {
	Schema      string          `json:"$schema"`
	Name        string          `json:"name"`
	Version     string          `json:"version,omitempty"`
	Description string          `json:"description,omitempty"`
	Author      *Author         `json:"author,omitempty"`
	Homepage    string          `json:"homepage,omitempty"`
	Repository  string          `json:"repository,omitempty"`
	License     string          `json:"license,omitempty"`
	Keywords    []string        `json:"keywords,omitempty"`
	Extensions  json.RawMessage `json:"extensions,omitempty"`
}

// knownFields 是闭合 schema 允许的顶层字段，用来把多出来的挑出来报告。
var knownFields = map[string]bool{
	"$schema": true, "name": true, "version": true, "description": true,
	"author": true, "homepage": true, "repository": true, "license": true,
	"keywords": true, "extensions": true,
}

// validPluginName 是标准对 name 的要求：小写字母数字、连字符与点，
// 首尾必须是字母或数字。连续的连字符与点另行检查——正则写得下，但写下去之后
// 没人看得懂它在拒什么。
var validPluginName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$|^[a-z0-9]$`)

// ParseManifest 解析并校验 plugin.json。
//
// 返回的 warnings 是「不影响加载但该让人知道」的事（未知字段、extensions 不是
// 对象）；返回 error 才表示这个插件包不能用。两者的界线由标准划定，不是本实现
// 的取舍：schema 不认识、name 不合法是致命的，其余一律往下走。
func ParseManifest(lang string, data []byte) (Manifest, []string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Manifest{}, nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.manifest_json", "%s 不是合法的 JSON：%w"), ManifestFile, err)
	}

	var warnings []string
	for k := range raw {
		if !knownFields[k] {
			warnings = append(warnings, i18n.Tf(lang, "plugin.bundle.warn.unknown_field",
				"%s 里有标准未定义的字段 %q，已忽略", ManifestFile, k))
		}
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, warnings, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.manifest_types", "%s 的字段类型不对：%w"), ManifestFile, err)
	}

	if m.Schema == "" {
		return Manifest{}, warnings, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.no_schema", "%s 里缺少 $schema"), ManifestFile)
	}
	if m.Schema != SchemaURL {
		return Manifest{}, warnings, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.bad_schema", "不支持的 $schema %q（本程序支持 %s）"), m.Schema, SchemaURL)
	}
	if err := checkName(lang, m.Name); err != nil {
		return Manifest{}, warnings, err
	}

	// extensions 是各客户端的私有命名空间。不是对象时只报告不报废：本程序一个
	// 命名空间都不实现，它写成什么样都影响不到这里。
	if len(m.Extensions) > 0 {
		var probe map[string]json.RawMessage
		if json.Unmarshal(m.Extensions, &probe) != nil {
			warnings = append(warnings, i18n.T(lang, "plugin.bundle.warn.bad_extensions", "extensions 不是对象，已忽略"))
			m.Extensions = nil
		}
	}
	return m, warnings, nil
}

// checkName 按标准校验插件名。
func checkName(lang, name string) error {
	switch {
	case name == "":
		return fmt.Errorf(i18n.T(lang, "plugin.bundle.err.no_name", "%s 里缺少 name"), ManifestFile)
	case len(name) > 64:
		return fmt.Errorf(i18n.T(lang, "plugin.bundle.err.name_long", "插件名 %q 超过 64 个字符"), name)
	case !validPluginName.MatchString(name):
		return fmt.Errorf("插件名 %q 不合法（只允许小写字母、数字、连字符与点，且首尾必须是字母或数字）", name)
	case strings.Contains(name, "--"), strings.Contains(name, ".."):
		return fmt.Errorf("插件名 %q 不合法（不允许连续的连字符或点）", name)
	}
	return nil
}
