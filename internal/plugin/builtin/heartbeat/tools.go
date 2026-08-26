package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wen/internal/i18n"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

type setIntervalTool struct{ p *Plugin }

func (t *setIntervalTool) Name() string { return "set_heartbeat_interval" }

func (t *setIntervalTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.heartbeat.tool.set_heartbeat_interval.desc", setDescZH)
}

const setDescZH = "设定隔多久之后再主动开口找用户，调用后倒计时从此刻重新开始。" +
	"话题正热、或者答应了要跟进某件事，就把它调短；" +
	"对方说要去忙、要去睡，或者眼下确实没什么可跟进的，就调长。" +
	"配置了情境提示词时，可顺带用 context 声明当下的情境，下次醒来按那套提示词行事。" +
	"拿不准时不必调用——不调用就沿用当前节奏。"

// Schema 的 context 用配置生成的 enum。Agent 每次请求都重新取工具声明，
// 因此改了情境配置即时生效。
func (t *setIntervalTool) Schema() json.RawMessage {
	lang := t.p.langOf()
	return json.RawMessage(fmt.Sprintf(
		prompt.T(lang, "prompt.heartbeat.tool.set_heartbeat_interval.schema", setSchemaZH),
		contextSchemaFragment(lang, t.p,
			prompt.Tf(lang, "prompt.heartbeat.context_desc.set",
				"当下的情境，决定下次心跳用哪套提示词；不传则沿用当前情境，传「%s」回到默认提示词",
				contexts.For(lang, contextDefault)))))
}

const setSchemaZH = `{
	"type": "object",
	"properties": {
		"minutes": {"type": "integer", "description": "多少分钟之后再主动开口"},
		"reason": {"type": "string", "description": "这样定的理由，一句话，如「她说要去睡了」"}%s
	},
	"required": ["minutes", "reason"]
}`

// contextSchemaFragment 生成 context 参数的 schema 片段（自带逗号前缀）。
// 没配置任何情境时返回空串不声明该参数——一个只有保留值的参数只会诱导模型去传它。
func contextSchemaFragment(lang string, p *Plugin, desc string) string {
	names := contextNames(p.snapshotContexts())
	if len(names) == 0 {
		return ""
	}
	// 情境名是用户自己写的，原样给出；只有保留名「默认」按语言译
	enum, err := json.Marshal(append(names, contexts.For(lang, contextDefault)))
	if err != nil {
		return ""
	}
	return `,
		"context": {"type": "string", "description": "` + desc + `", "enum": ` + string(enum) + `}`
}

// applyContextLocked 按名字切换当前情境，返回接在工具回显里的说明；空名不动。
// 两个工具共用（set_heartbeat_interval 与 pause_heartbeat）。调用方需持有 p.mu。
func (p *Plugin) applyContextLocked(name string) (string, error) {
	lang := p.lang
	if name == "" {
		return "", nil
	}
	// 保留名认任意语言：模型照着 Schema 里的译名回传，也可能照着历史里的中文旧值
	if zh, ok := contexts.Canonical(name); ok && zh == contextDefault {
		p.curContext = ""
		return prompt.T(lang, "prompt.heartbeat.result.context_default", "，情境回到默认"), nil
	}
	if _, ok := p.contexts[name]; !ok {
		names := contextNames(p.contexts)
		if len(names) == 0 {
			return "", fmt.Errorf("%s", prompt.T(lang, "prompt.heartbeat.err.no_contexts",
				"尚未配置任何情境提示词，context 参数不可用"))
		}
		return "", fmt.Errorf(prompt.T(lang, "prompt.heartbeat.err.no_such_context",
			"没有名为 %q 的情境，可选：%s"), name,
			strings.Join(names, prompt.T(lang, "prompt.heartbeat.context_sep", "、")))
	}
	p.curContext = name
	return prompt.Tf(lang, "prompt.heartbeat.result.context_switched", "，情境切到「%s」", name), nil
}

func (t *setIntervalTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Minutes int    `json:"minutes"`
		Reason  string `json:"reason"`
		Context string `json:"context"`
	}
	p := t.p
	lang := p.langOf()
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	if a.Minutes <= 0 {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.heartbeat.err.bad_minutes",
			"间隔要是一个正整数分钟"))
	}

	p.mu.Lock()
	if !p.dynamic {
		cur := p.cur
		p.mu.Unlock()
		// 拦截生效时要把规则说清楚，否则模型只会换个数字再试一次
		return "", fmt.Errorf(prompt.T(lang, "prompt.heartbeat.err.fixed_pace",
			"当前是固定节奏（每 %s），节奏不可调整。如需可调，请在设置页打开心跳插件的「动态心跳」"),
			humanDur(lang, cur))
	}

	ctxNote, err := p.applyContextLocked(a.Context)
	if err != nil {
		p.mu.Unlock()
		return "", err
	}

	want := time.Duration(a.Minutes) * time.Minute
	p.cur = p.normalize(want)
	p.adjusted = true
	// 倒计时从此刻重算：「N 分钟之后再开口」说的是从现在起算。resetClockLocked
	// 只前推不回拨，因此这里不会把已经临近的一次心跳推回过去。
	p.resetClockLocked(time.Now())
	next, minIv, maxIv := p.cur, p.minIv, p.maxIv
	dir, st := p.snapshotStateLocked()
	p.mu.Unlock()

	// 同步写：Execute 本就在轮次的同步路径上，另起 goroutine 会让这次写盘
	// 脱离插件的生命周期
	persistState(dir, st)
	i18n.Logf(t.p.langOf(), "log.heartbeat.interval_set", "heartbeat: 模型设定心跳间隔为 %v（%s）", next, a.Reason)

	out := prompt.Tf(lang, "prompt.heartbeat.result.set", "好，下次主动开口在 %s后%s。",
		humanDur(lang, next), ctxNote)
	if next != want {
		out += prompt.Tf(lang, "prompt.heartbeat.result.clamped",
			"（你报的是 %d 分钟，已按设置的范围 %s ~ %s 收到 %s）",
			a.Minutes, humanDur(lang, minIv), humanDur(lang, maxIv), humanDur(lang, next))
	}
	return out, nil
}

// snapshotContexts 取一份情境表快照（Schema 生成用）。
func (p *Plugin) snapshotContexts() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.contexts
}

// ---------- pause_heartbeat ----------

// maxPauseMinutes 限制单次暂停时长：暂停不是关闭，睡一觉的量级封顶。
const maxPauseMinutes = 24 * 60

type pauseTool struct{ p *Plugin }

func (t *pauseTool) Name() string { return "pause_heartbeat" }

func (t *pauseTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.heartbeat.tool.pause_heartbeat.desc", pauseDescZH)
}

const pauseDescZH = "暂停主动开口一段时间，到点恢复常规节奏（睡醒回到平常状态）——对方说要去睡、" +
	"要出门长时间离开时用它，免得在不合适的时候被定时唤醒。" +
	"期间对方随时来消息不受影响，且对方一说话暂停即自动取消。" +
	"配置了情境提示词时，可用 context 指定醒来后按哪套提示词开口（如睡前暂停到早上，指定早晨的情境）。"

func (t *pauseTool) Schema() json.RawMessage {
	lang := t.p.langOf()
	return json.RawMessage(fmt.Sprintf(
		prompt.T(lang, "prompt.heartbeat.tool.pause_heartbeat.schema", pauseSchemaZH),
		maxPauseMinutes,
		contextSchemaFragment(lang, t.p,
			prompt.Tf(lang, "prompt.heartbeat.context_desc.pause",
				"暂停结束醒来时用的情境；不传则沿用当前情境，传「%s」回到默认提示词",
				contexts.For(lang, contextDefault)))))
}

const pauseSchemaZH = `{
	"type": "object",
	"properties": {
		"minutes": {"type": "integer", "description": "暂停多少分钟（最长 %d，即一天）。睡一夜按 420-540 报"},
		"reason": {"type": "string", "description": "暂停的理由，一句话，如「她说要睡了」"}%s
	},
	"required": ["minutes", "reason"]
}`

func (t *pauseTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Minutes int    `json:"minutes"`
		Reason  string `json:"reason"`
		Context string `json:"context"`
	}
	p := t.p
	lang := p.langOf()
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	if a.Minutes <= 0 {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.heartbeat.err.bad_pause",
			"暂停时长要是一个正整数分钟"))
	}
	capped := min(a.Minutes, maxPauseMinutes)

	p.mu.Lock()
	if !p.dynamic {
		cur := p.cur
		p.mu.Unlock()
		return "", fmt.Errorf(prompt.T(lang, "prompt.heartbeat.err.fixed_pause",
			"当前是固定节奏（每 %s），不可暂停。如需可调，请在设置页打开心跳插件的「动态心跳」"),
			humanDur(lang, cur))
	}
	// 情境先于暂停生效：情境名不对时整个调用失败，不能只暂停了一半——
	// 那会让模型以为「醒来换情境」已经安排好了
	ctxNote, err := p.applyContextLocked(a.Context)
	if err != nil {
		p.mu.Unlock()
		return "", err
	}
	now := time.Now()
	p.pausedAt = now
	p.pausedUntil = now.Add(time.Duration(capped) * time.Minute)
	// 循环可能正挂在一个更早的到期时刻上，叫醒它按暂停重算
	select {
	case p.wake <- struct{}{}:
	default:
	}
	until := p.pausedUntil
	dir, st := p.snapshotStateLocked()
	p.mu.Unlock()

	persistState(dir, st)
	i18n.Logf(t.p.langOf(), "log.heartbeat.paused", "heartbeat: 模型暂停心跳至 %s（%s）", until.Format("2006-01-02 15:04"), a.Reason)

	out := prompt.Tf(lang, "prompt.heartbeat.result.paused",
		"好，接下来 %s内不再主动开口，到点恢复常规节奏%s；对方一说话即恢复。",
		humanDur(lang, time.Duration(capped)*time.Minute), ctxNote)
	if capped != a.Minutes {
		out += prompt.Tf(lang, "prompt.heartbeat.result.pause_capped",
			"（你报的是 %d 分钟，已按上限收到 %d 分钟）", a.Minutes, capped)
	}
	return out, nil
}
