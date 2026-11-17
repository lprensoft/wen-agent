// Package media 提供媒体出站的系统插件：send_image 与 send_file 让模型把工作
// 目录里的图片或文件发给这个会话的收件人。出口跟着这一轮的文字走——前台 IM
// 轮次回给对面，心跳等后台轮次推给绑定了该会话的用户，前台界面轮次没有外部
// 出口。图片还会经核心记进会话，网页界面因此看得到她发出去的照片。
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
	return "让模型把工作目录里的图片或文件发给这个会话的收件人（消息通道与后台轮次都可以；图片同时留在对话里，网页界面可见）"
}

func (p *Plugin) SystemPrompt() string { return "" }

func (p *Plugin) ConfigFields() []plugin.ConfigField { return nil }

func (p *Plugin) Init(ictx plugin.InitContext, _ map[string]any) error {
	p.workdir = ictx.Workdir
	p.lang = ictx.Lang
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&sendImageTool{p: p}, &sendFileTool{p: p}}
}
