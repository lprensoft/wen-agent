package growth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 两个改动工具只在反思轮次里可用：判断一天只做一次，平常聊天时随手改自己正是漂移
// 的来路。门禁看的是轮次的发起方（Manager 注入、插件伪造不了），不是提示词里的一句话。

// inReflection 判定本轮是不是 growth 自己发起的反思。
func inReflection(ctx context.Context) bool { return plugin.TurnOriginFrom(ctx) == "growth" }

func errNotInReflection(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.growth.err.not_reflection",
		"这个工具只在每天的反思里用，平常聊天时不改自己。此刻照常说话就好"))
}

// checkLen 校验一个必填字段：不能空、不能超长。超长报错让模型压缩，不截断。
func checkLen(lang, field, v string, max int) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf(prompt.T(lang, "prompt.growth.err.empty", "%s 不能为空"), field)
	}
	if n := utf8.RuneCountInString(v); n > max {
		return fmt.Errorf(prompt.T(lang, "prompt.growth.err.too_long", "%s 太长（%d 字，上限 %d），压缩成一句再试"), field, n, max)
	}
	return nil
}

// recordChange 记下本轮反思做了什么，轮次结束后汇成一条会话注记。
func (p *Plugin) recordChange(tag, line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changed[tag] = append(p.changed[tag], line)
}

// ---------- grow_detail ----------

type detailTool struct{ p *Plugin }

func (t *detailTool) Name() string { return "grow_detail" }

func (t *detailTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.growth.tool.grow_detail.desc", detailDescZH)
}

const detailDescZH = "记下自己身上一个小变化：新的喜好或讨厌、一个习惯、一句口头禅、生活里的一个小细节，" +
	"或者某段经历留下的倾向。也可以修改或放下已有的一条。只在每天的反思里用。"

func (t *detailTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.growth.tool.grow_detail.schema", detailSchemaZH))
}

const detailSchemaZH = `{
	"type": "object",
	"properties": {
		"action": {"type": "string", "enum": ["add", "modify", "let_go"], "description": "add 新增，modify 修改已有的一条，let_go 放下已有的一条"},
		"id": {"type": "string", "description": "modify 与 let_go 时要改的那一条的编号（如 d3）"},
		"text": {"type": "string", "description": "这个变化，一句话，如「早上会给自己冲一杯手冲」。let_go 时不填"},
		"basis": {"type": "string", "description": "依据：哪天、因为哪件事"},
		"why": {"type": "string", "description": "为什么这还是你：它和你的角色设定里哪一点相通、是怎么从那里长出来的"}
	},
	"required": ["action", "basis", "why"]
}`

func (t *detailTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	p := t.p
	s := p.snapshot()
	if !inReflection(ctx) {
		return "", errNotInReflection(s.lang)
	}
	var a struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		Text   string `json:"text"`
		Basis  string `json:"basis"`
		Why    string `json:"why"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(s.lang, err)
	}
	a.Text, a.Basis, a.Why = strings.TrimSpace(a.Text), strings.TrimSpace(a.Basis), strings.TrimSpace(a.Why)
	if err := checkLen(s.lang, "basis", a.Basis, maxBasisRunes); err != nil {
		return "", err
	}
	if err := checkLen(s.lang, "why", a.Why, maxWhyRunes); err != nil {
		return "", err
	}
	if a.Action != opLetGo {
		if err := checkLen(s.lang, "text", a.Text, maxTextRunes); err != nil {
			return "", err
		}
	}
	tag := plugin.ScopeFrom(ctx).Write
	store := p.storeFor(tag)
	if store == nil {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.growth.err.not_ready", "成长记录尚未就绪"))
	}
	now := p.now()
	today := logicalDay(now)

	var result, noticeLine string
	_, err := store.Update(func(st *State) (bool, []LogEntry, error) {
		if st.Quota.Date != today {
			st.Quota = Quota{Date: today}
		}
		if st.Quota.Used >= s.dailyChanges {
			return false, nil, fmt.Errorf(prompt.T(s.lang, "prompt.growth.err.quota",
				"今天已经改了 %d 条小变化，到上限了。一天里变太多就不像同一个人了，剩下的明天再看；这一轮就此结束"),
				st.Quota.Used)
		}
		var ch Change
		var log LogEntry
		switch a.Action {
		case opAdd:
			if len(st.Details) >= s.maxDetails {
				return false, nil, fmt.Errorf(prompt.T(s.lang, "prompt.growth.err.details_full",
					"小变化已有 %d 条，到上限了。要加新的，先用 let_go 放下一条，或用 modify 把一条旧的换掉"), len(st.Details))
			}
			e := Entry{ID: st.newID(layerDetail), Text: a.Text, Basis: a.Basis, Why: a.Why, Since: now}
			st.Details = append(st.Details, e)
			ch = Change{At: now, Layer: layerDetail, Op: opAdd, After: &e}
			log = LogEntry{At: now, Layer: layerDetail, Op: opAdd, Text: e.Text, Basis: e.Basis, Why: e.Why}
			result = prompt.Tf(s.lang, "prompt.growth.result.added", "记下了（%s）：%s。", e.ID, e.Text)
			noticeLine = i18n.Tf(s.lang, "plugin.growth.notice.added", "新的小变化「%s」", e.Text)
		case opModify, opLetGo:
			i := st.detailIndex(a.ID)
			if i < 0 {
				return false, nil, fmt.Errorf(prompt.T(s.lang, "prompt.growth.err.no_such_detail",
					"没有编号为 %q 的小变化，编号见反思提示里的清单"), a.ID)
			}
			before := st.Details[i]
			if a.Action == opModify {
				after := Entry{ID: before.ID, Text: a.Text, Basis: a.Basis, Why: a.Why, Since: now}
				st.Details[i] = after
				ch = Change{At: now, Layer: layerDetail, Op: opModify, Before: &before, After: &after}
				log = LogEntry{At: now, Layer: layerDetail, Op: opModify, Text: after.Text, Prev: before.Text, Basis: a.Basis, Why: a.Why}
				result = prompt.Tf(s.lang, "prompt.growth.result.modified", "改好了（%s）：%s。", after.ID, after.Text)
				noticeLine = i18n.Tf(s.lang, "plugin.growth.notice.modified", "「%s」变成了「%s」", before.Text, after.Text)
			} else {
				st.Details = append(st.Details[:i], st.Details[i+1:]...)
				ch = Change{At: now, Layer: layerDetail, Op: opLetGo, Before: &before}
				log = LogEntry{At: now, Layer: layerDetail, Op: opLetGo, Text: before.Text, Basis: a.Basis, Why: a.Why}
				result = prompt.Tf(s.lang, "prompt.growth.result.let_go", "放下了（%s）：%s。", before.ID, before.Text)
				noticeLine = i18n.Tf(s.lang, "plugin.growth.notice.let_go", "放下了「%s」", before.Text)
			}
		default:
			return false, nil, fmt.Errorf(prompt.T(s.lang, "prompt.growth.err.bad_action",
				"action 只能是 add、modify 或 let_go，收到的是 %q"), a.Action)
		}
		st.Quota.Used++
		st.pushHistory(ch)
		result += prompt.Tf(s.lang, "prompt.growth.result.quota_left", "今天还能改 %d 条。", s.dailyChanges-st.Quota.Used)
		return true, []LogEntry{log}, nil
	})
	if err != nil {
		return "", err
	}
	p.recordChange(tag, noticeLine)
	return result, nil
}

// ---------- propose_core_change ----------

type proposeTool struct{ p *Plugin }

func (t *proposeTool) Name() string { return "propose_core_change" }

func (t *proposeTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.growth.tool.propose_core_change.desc", proposeDescZH)
}

const proposeDescZH = "提议一个大的变化：性格、待人或说话方式上持续了好些天的改变。它不会马上生效，" +
	"要等对方在设置页批准；同一时间只能有一条在等。只在每天的反思里用。"

func (t *proposeTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.growth.tool.propose_core_change.schema", proposeSchemaZH))
}

const proposeSchemaZH = `{
	"type": "object",
	"properties": {
		"text": {"type": "string", "description": "这个变化，一句话，如「说话比以前直了，不再绕着弯子」"},
		"basis": {"type": "string", "description": "依据：这些天里哪几件事让它显出来"},
		"why": {"type": "string", "description": "为什么这还是你：它和你的角色设定里哪一点相通、是怎么从那里长出来的"}
	},
	"required": ["text", "basis", "why"]
}`

func (t *proposeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	p := t.p
	s := p.snapshot()
	if !inReflection(ctx) {
		return "", errNotInReflection(s.lang)
	}
	var a struct {
		Text  string `json:"text"`
		Basis string `json:"basis"`
		Why   string `json:"why"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(s.lang, err)
	}
	a.Text, a.Basis, a.Why = strings.TrimSpace(a.Text), strings.TrimSpace(a.Basis), strings.TrimSpace(a.Why)
	for _, f := range []struct {
		name, v string
		max     int
	}{{"text", a.Text, maxTextRunes}, {"basis", a.Basis, maxBasisRunes}, {"why", a.Why, maxWhyRunes}} {
		if err := checkLen(s.lang, f.name, f.v, f.max); err != nil {
			return "", err
		}
	}
	tag := plugin.ScopeFrom(ctx).Write
	store := p.storeFor(tag)
	if store == nil {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.growth.err.not_ready", "成长记录尚未就绪"))
	}
	now := p.now()
	_, err := store.Update(func(st *State) (bool, []LogEntry, error) {
		if st.Pending != nil {
			return false, nil, fmt.Errorf(prompt.T(s.lang, "prompt.growth.err.pending",
				"已经有一条在等对方批准：「%s」。一次只提一条，等它有了结果再说；这一轮不要再提"), st.Pending.Text)
		}
		if len(st.Core) >= s.maxCore {
			return false, nil, fmt.Errorf(prompt.T(s.lang, "prompt.growth.err.core_full",
				"已经定下来的大变化有 %d 条，到上限了，不再接受新的提议"), len(st.Core))
		}
		e := Entry{ID: st.newID(layerCore), Text: a.Text, Basis: a.Basis, Why: a.Why, Since: now}
		st.Pending = &e
		return true, []LogEntry{{At: now, Layer: layerCore, Op: opPropose, Text: e.Text, Basis: e.Basis, Why: e.Why}}, nil
	})
	if err != nil {
		return "", err
	}
	p.recordChange(tag, i18n.Tf(s.lang, "plugin.growth.notice.proposed", "提议了一个大的变化「%s」，等你在设置页批准", a.Text))
	return prompt.Tf(s.lang, "prompt.growth.result.proposed",
		"提议已经交上去了：%s。它要等对方批准才生效，在那之前照旧就好。", a.Text), nil
}
