package agentplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// remoteTool 把一件远端工具接成本项目的工具。
type remoteTool struct {
	p    *Plugin
	info ToolInfo
}

func (t *remoteTool) Name() string { return t.info.Local }

// Description 原样透传远端给的说明。
//
// 不改写也不翻译：这段字是模型用来判断「该不该调这件工具」的全部依据，我们既不知道
// 那件工具究竟做什么，也没有比作者更好的说法。它多半是英文——本项目的提示词一律中文
// 这条约定管的是**我们自己写的**提示词，外部数据不在其中，译一遍反而可能译错。
func (t *remoteTool) Description() string {
	desc := strings.TrimSpace(t.info.Description)
	if desc == "" {
		desc = prompt.Tf(t.p.langOf(), "prompt.bundle.tool.no_desc",
			"外源插件包 %s 提供的工具（对方没有给出说明）", t.p.bundle.Manifest.Name)
	}
	return desc
}

// Schema 原样透传远端的参数 schema；缺失时给一个空对象，免得模型无从下手。
func (t *remoteTool) Schema() json.RawMessage {
	if len(t.info.Schema) > 0 {
		return t.info.Schema
	}
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *remoteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	p := t.p
	p.mu.RLock()
	c, ok := p.clients[t.info.Server]
	lang := p.lang
	maxBytes, needConfirm, ready := p.maxBytes, p.confirm, p.ready
	p.mu.RUnlock()

	if !ready {
		return "", fmt.Errorf(prompt.T(lang, "prompt.bundle.err.not_ready", "插件包 %s 尚未就绪"), p.bundle.Manifest.Name)
	}
	if !ok {
		return "", fmt.Errorf(prompt.T(lang, "prompt.bundle.err.no_server", "插件包 %s 里没有可用的 %s，这件工具暂时用不了"),
			p.bundle.Manifest.Name, t.info.Server)
	}

	var parsed map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return "", plugin.BadArgs(lang, err)
		}
	}

	if needConfirm {
		if err := t.askFirst(ctx, args); err != nil {
			return "", err
		}
	}

	res, err := c.call(ctx, t.info.Remote, parsed)
	if err != nil {
		return "", err
	}
	out := truncate(lang, renderResult(lang, res), maxBytes)
	if res.IsError {
		// 远端把失败放在结果里而不是协议错误里，正是为了让模型看见并自行纠正，
		// 所以照常返回内容，只在前面点明这是一次失败。
		return prompt.Tf(lang, "prompt.bundle.result.failed", "调用失败：\n%s", out), nil
	}
	if out == "" {
		return prompt.T(lang, "prompt.bundle.result.empty", "（这次调用没有返回内容）"), nil
	}
	return out, nil
}

// askFirst 在调用之前请人过目。
//
// 拿不到答复一律按拒绝：确认通道取不到（无人值守的后台轮次）、超时、界面断开，
// 三种情况都不等于同意。要放行就把这个开关关掉，那是个看得见的选择。
func (t *remoteTool) askFirst(ctx context.Context, args json.RawMessage) error {
	lang := t.p.langOf()
	confirm, ok := plugin.ConfirmerFrom(ctx)
	if !ok {
		return fmt.Errorf("%s", prompt.T(lang, "prompt.bundle.err.no_confirmer",
			"这个插件包设了调用前确认，而当前轮次没有可确认的人，已按拒绝处理"))
	}
	detail := t.info.Local
	if len(args) > 0 {
		detail += "\n" + string(args)
	}
	agreed, err := confirm(ctx, plugin.ConfirmRequest{
		Source: t.p.name,
		Title:  i18n.T(lang, "plugin.bundle.confirm.title", "调用外源工具"),
		Detail: detail,
		Reason: i18n.Tf(lang, "plugin.bundle.confirm.reason", "插件包 %s 开着「调用前确认」", t.p.bundle.Manifest.Name),
	})
	if err != nil {
		return fmt.Errorf(prompt.T(lang, "prompt.bundle.err.confirm_failed", "没能取得确认（%v），已按拒绝处理"), err)
	}
	if !agreed {
		return fmt.Errorf("%s", prompt.T(lang, "prompt.bundle.err.denied", "用户拒绝了这次调用"))
	}
	return nil
}

// truncate 按字节数截断，不切坏多字节字符，并明说被截过。
//
// 明说是必要的：截断之后的内容看起来是完整的一段，模型会拿它当全部事实往下推。
func truncate(lang, s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + prompt.Tf(lang, "prompt.bundle.result.truncated",
		"\n\n（内容过长，已截断；这里只有前 %d 字节，完整内容共 %d 字节）", cut, len(s))
}

// utf8Start 报告这个字节是不是一个字符的起始字节。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
