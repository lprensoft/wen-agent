// Package reaction 提供表情回应插件：模型可以在对方刚发来的那条消息上加一个表情
// （赞、笑、惊讶等），像人看到消息时随手点的那样。它只在消息通道发起的轮次里可用，
// 具体怎么映射成平台的表情由各通道自己决定。
package reaction

import (
	"wen/internal/plugin"
)

// Plugin 是 reaction 系统插件（不注入提示词）。
type Plugin struct {
	lang string
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "reaction" }

func (p *Plugin) Category() string { return plugin.CategoryChannel }

func (p *Plugin) Description() string {
	return "让模型在对方的消息上加一个表情回应（赞、笑、惊讶等），与文字回复并存或单独使用；只在消息通道发起的对话里可用，支持的通道见「消息通道」说明"
}

func (p *Plugin) SystemPrompt() string { return "" }

func (p *Plugin) ConfigFields() []plugin.ConfigField { return nil }

func (p *Plugin) Init(ictx plugin.InitContext, _ map[string]any) error {
	p.lang = ictx.Lang
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&reactTool{p: p}}
}
