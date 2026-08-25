// Package readfile 提供读取本地文本文件的系统插件。
package readfile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

const defaultMaxBytes = 64 * 1024

// Plugin 是 read_file 系统插件（不注入提示词）。
type Plugin struct {
	workdir  string
	maxBytes int
	lang     string
}

func New() *Plugin { return &Plugin{maxBytes: defaultMaxBytes} }

func (p *Plugin) Name() string { return "read_file" }

func (p *Plugin) Category() string     { return plugin.CategoryTools }
func (p *Plugin) Description() string  { return "读取本地文本文件内容" }
func (p *Plugin) SystemPrompt() string { return "" }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{{
		Key:         "max_bytes",
		Label:       "最大读取字节数",
		Type:        plugin.FieldInt,
		Description: "单次读取返回的最大字节数，超出部分截断。",
		Default:     defaultMaxBytes,
		Min:         plugin.IntPtr(1024),
		Max:         plugin.IntPtr(4 * 1024 * 1024),
	}}
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	p.workdir = ictx.Workdir
	p.lang = ictx.Lang
	p.maxBytes = plugin.CfgInt(cfg, "max_bytes", defaultMaxBytes)
	return nil
}

func (p *Plugin) Tools() []plugin.Tool { return []plugin.Tool{&tool{p: p}} }

type tool struct {
	p *Plugin
}

func (t *tool) Name() string { return "read_file" }

func (t *tool) Description() string {
	return prompt.T(t.p.lang, "prompt.read_file.tool.read_file.desc",
		"读取指定路径的文本文件内容。相对路径基于工作目录解析。文件过大时截断。")
}

// schemaZH 是参数 Schema。整段一个键：它是手写的 JSON，拆成一个字段一个键的话，
// 译者要照着结构去拼，而拼错一个引号就是一份非法的 Schema。
const schemaZH = `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "要读取的文件路径（绝对或相对）"}
		},
		"required": ["path"]
	}`

func (t *tool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.lang, "prompt.read_file.tool.read_file.schema", schemaZH))
}

func (t *tool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	path := a.Path
	if !filepath.IsAbs(path) && t.p.workdir != "" {
		path = filepath.Join(t.p.workdir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	data, truncated := plugin.TruncateUTF8(data, t.p.maxBytes)
	out := string(data)
	if truncated {
		out += prompt.Tf(t.p.lang, "prompt.read_file.result.truncated",
			"\n...[内容超过 %d 字节，已截断]", t.p.maxBytes)
	}
	return out, nil
}
