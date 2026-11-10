// Package media 提供媒体出站的系统插件。目前只有 send_image：让模型把工作目录
// 里的一张图片发回当前 IM 对话。Web UI 与后台轮次没有收件人与回复凭据，调用会
// 明确失败，不会悄悄发到别处。
package media

import (
	"wen/internal/plugin"
)

// Plugin 是 media 系统插件（不注入提示词）。
type Plugin struct {
	workdir string
	lang    string
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "media" }

func (p *Plugin) Category() string { return plugin.CategoryTools }

func (p *Plugin) Description() string {
	return "让模型把工作目录里的图片发回当前的 IM 对话（仅 IM 通道，Web UI 与后台轮次不可用）"
}

func (p *Plugin) SystemPrompt() string { return "" }

func (p *Plugin) ConfigFields() []plugin.ConfigField { return nil }

func (p *Plugin) Init(ictx plugin.InitContext, _ map[string]any) error {
	p.workdir = ictx.Workdir
	p.lang = ictx.Lang
	return nil
}

func (p *Plugin) Tools() []plugin.Tool { return []plugin.Tool{&sendImageTool{p: p}} }
