package reaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// vocabulary 是可选的表情，顺序即候选项的展示顺序。落盘之外的一次性动作，但仍用
// prompt.Enum：候选项要按语言给出，而回调与通道只认中文规范值。中文名与 imbot 的词表
// 逐字相同（见测试），这里写成字面量是为了让覆盖检查能看到它。
var vocabulary = prompt.NewEnum(
	prompt.Text("prompt.reaction.value.like", "赞"),
	prompt.Text("prompt.reaction.value.heart", "爱心"),
	prompt.Text("prompt.reaction.value.laugh", "笑"),
	prompt.Text("prompt.reaction.value.cry", "哭"),
	prompt.Text("prompt.reaction.value.surprise", "惊讶"),
	prompt.Text("prompt.reaction.value.thinking", "思考"),
	prompt.Text("prompt.reaction.value.celebrate", "庆祝"),
	prompt.Text("prompt.reaction.value.cheer", "加油"),
	prompt.Text("prompt.reaction.value.got_it", "收到"),
)

type reactTool struct{ p *Plugin }

func (t *reactTool) Name() string { return "react_to_message" }

func (t *reactTool) Description() string {
	return prompt.T(t.p.lang, "prompt.reaction.tool.react.desc",
		"在对方刚发来的那条消息上加一个表情回应，像人看到消息时随手点的那样。可以单独用（之后不必再写文字），也可以和文字回复一起用。一轮只能回应一次，回应过就不要再调用。只在消息通道发起的对话里可用；网页界面与后台轮次没有可回应的消息。")
}

const reactSchemaZH = `{
		"type": "object",
		"properties": {
			"reaction": {"type": "string", "enum": %[1]s, "description": "要加上的表情"}
		},
		"required": ["reaction"]
	}`

func (t *reactTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.Tf(t.p.lang, "prompt.reaction.tool.react.schema",
		reactSchemaZH, vocabulary.JSON(t.p.lang)))
}

func (t *reactTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Reaction string `json:"reaction"`
	}
	lang := t.p.lang
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	zh, ok := vocabulary.Canonical(a.Reaction)
	if !ok {
		return "", fmt.Errorf("%s", i18n.Tf(lang, "plugin.reaction.err.unknown",
			"没有「%s」这个表情，请从给出的选项里选。", a.Reaction))
	}

	react := plugin.ReplyReactionFrom(ctx)
	if react == nil {
		return "", fmt.Errorf("%s", i18n.T(lang, "plugin.reaction.err.no_channel",
			"当前对话不是从消息通道发起的，没有可以回应的消息。"))
	}
	if err := react(zh); err != nil {
		switch {
		case errors.Is(err, imbot.ErrReactionUnsupported):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.reaction.err.unsupported",
				"这条通道不支持表情回应。"))
		case errors.Is(err, imbot.ErrNoReactionTarget):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.reaction.err.no_target",
				"没有可以回应的消息。"))
		case errors.Is(err, imbot.ErrReactionAlreadySent):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.reaction.err.already",
				"这一轮已经回应过了，不必再回应。"))
		default:
			// 平台错误原文只进日志，不进模型上下文。
			i18n.Logf(lang, "log.reaction.send_failed", "reaction: 表情回应失败: %v", err)
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.reaction.err.send", "表情回应发送失败。"))
		}
	}
	return i18n.Tf(lang, "plugin.reaction.sent",
		"已给对方的消息加上「%s」表情，本轮不必再回应。", vocabulary.For(lang, zh)), nil
}
