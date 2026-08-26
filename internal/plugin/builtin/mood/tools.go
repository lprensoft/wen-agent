package mood

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// errNotReady 在插件未取得持久化目录时返回，正常流程下不会出现。
func errNotReady(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.mood.result.not_ready", "心情尚未就绪"))
}

type adjustTool struct{ p *Plugin }

func (t *adjustTool) Name() string { return "adjust_mood" }

func (t *adjustTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.mood.tool.adjust_mood.desc",
		"按本轮互动调整角色此刻的心情，并返回调整后的心情。"+
			"报的是变化量而不是目标值：正数变开心、负数变沮丧。"+
			"出现会影响心情的事时先调用它拿到新的心情，再据此写这一轮的表达；一轮最多调一次。")
}

func (t *adjustTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.mood.tool.adjust_mood.schema", schemaZH))
}

// schemaZH 是参数 Schema。整段一个键：它是手写的 JSON，拆成一个字段一个键的话，
// 译者要照着结构去拼，而拼错一个引号就是一份非法的 Schema。
const schemaZH = `{
		"type": "object",
		"properties": {
			"delta": {"type": "integer", "description": "心情的变化量，正数变开心、负数变沮丧。小事几点，真正触动的事几十点"},
			"reason": {"type": "string", "description": "这次变化的起因，一句话，如「她夸了新剪的头发」"}
		},
		"required": ["delta", "reason"]
	}`

func (t *adjustTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Delta  int    `json:"delta"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}

	s := t.p.snapshot()
	// 心情只读写本轮的写入域：两个人格各有各的心情
	store := t.p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", errNotReady(s.lang)
	}
	st, applied, err := store.Apply(a.Delta, s.maxDelta, s.decayPerHour, a.Reason, time.Now())
	if err != nil {
		return "", err
	}

	out := prompt.Tf(s.lang, "prompt.mood.result.state", "心情：%s（%+d，本次 %+d）",
		band(s.lang, st.Value), st.Value, applied)
	// 拦截生效时要把规则告诉模型，否则它只会换个更大的数字再试一次
	if applied != a.Delta {
		out += prompt.Tf(s.lang, "prompt.mood.result.capped",
			"。本次变化量按单次上限 ±%d 收了（原报 %+d）", s.maxDelta, a.Delta)
	}
	return out + prompt.T(s.lang, "prompt.mood.result.period", "。"), nil
}
