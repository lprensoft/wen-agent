package unspoken

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// errNotReady 在插件未取得持久化目录时返回，正常流程下不会出现。
func errNotReady(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.unspoken.not_ready", "心里话尚未就绪"))
}

// ---------- keep_unspoken ----------

type keepTool struct{ p *Plugin }

func (t *keepTool) Name() string { return "keep_unspoken" }

func (t *keepTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.unspoken.tool.keep_unspoken.desc", keepDescZH)
}

const keepDescZH = "记一条没说出口的心里话：对对方的真实看法、憋着的话、在等的事、决定暂时不提的事。" +
	"只记会在心里留几天的事，不是每轮都记；一轮至多一条。清单满了会自动放下最早的一条。"

func (t *keepTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.unspoken.tool.keep_unspoken.schema", keepSchemaZH))
}

const keepSchemaZH = `{
	"type": "object",
	"properties": {
		"text": {"type": "string", "description": "这句心里话，一句（80 字内），如「他忘了纪念日，说不在意其实还在意」"}
	},
	"required": ["text"]
}`

func (t *keepTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Text string `json:"text"`
	}
	s := t.p.snapshot()
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(s.lang, err)
	}
	// 清单只读写本轮的写入域：心里话属于人格
	store := t.p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", errNotReady(s.lang)
	}
	res, err := store.Keep(a.Text, time.Now(), s.maxEntries)
	if err != nil {
		return "", err
	}
	if res.Duplicate {
		return prompt.Tf(s.lang, "prompt.unspoken.result.duplicate", "已经记着这一条了（第 %d 条）。", res.Index), nil
	}
	log.Printf("unspoken: 记下一条（第 %d 条）", res.Index)
	out := prompt.Tf(s.lang, "prompt.unspoken.result.kept", "已记下（第 %d 条）。", res.Index)
	if len(res.Dropped) > 0 {
		out += prompt.Tf(s.lang, "prompt.unspoken.result.dropped",
			"清单已满（上限 %d 条），放下了最早的：「%s」。", s.maxEntries,
			strings.Join(res.Dropped, prompt.T(s.lang, "prompt.unspoken.result.dropped_sep", "」「")))
	}
	return out, nil
}

// ---------- let_go ----------

type letGoTool struct{ p *Plugin }

func (t *letGoTool) Name() string { return "let_go" }

func (t *letGoTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.unspoken.tool.let_go.desc", letGoDescZH)
}

const letGoDescZH = "放下一条心里话：说出口了、想通了、不再在意了。用序号（按[心里话]里的顺序从 1 数起）或原文片段指明是哪一条。"

func (t *letGoTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.unspoken.tool.let_go.schema", letGoSchemaZH))
}

const letGoSchemaZH = `{
	"type": "object",
	"properties": {
		"index": {"type": "integer", "description": "要放下的那条的序号，从 1 数起"},
		"text": {"type": "string", "description": "要放下的那条的原文片段，能唯一定位即可；与 index 二选一"}
	}
}`

func (t *letGoTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Index int    `json:"index"`
		Text  string `json:"text"`
	}
	lang := t.p.langOf()
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", plugin.BadArgs(lang, err)
		}
	}
	store := t.p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", errNotReady(lang)
	}
	removed, err := store.LetGo(a.Index, a.Text)
	if err != nil {
		return "", err
	}
	log.Printf("unspoken: 放下一条")
	return prompt.Tf(lang, "prompt.unspoken.result.let_go", "已放下：「%s」。", removed.Text), nil
}
