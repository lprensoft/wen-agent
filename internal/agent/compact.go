package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/prompt"
	"wen/internal/session"
)

// EstimateHistoryTokens 估算一组消息的 token 总量（粗略值，供状态展示与预算裁剪）。
func EstimateHistoryTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateTokens(m)
	}
	return total
}

// EstimateStoredTokens 估算一段会话历史的 token 总量，会话注记不计入。
// 状态展示与压缩判据都该用它，别在调用处各自摊平一遍 StoredMessage。
func EstimateStoredTokens(stored []session.StoredMessage) int {
	return EstimateHistoryTokens(messagesOf(stored))
}

// messagesOf 剥掉存储层的附加字段，只留下消息本身。
// 会话注记不在其中：它从不发给模型，计进用量会让状态与压缩判据都虚高。
func messagesOf(stored []session.StoredMessage) []llm.Message {
	out := make([]llm.Message, 0, len(stored))
	for _, m := range stored {
		if m.Kind == session.KindNotice {
			continue
		}
		out = append(out, m.Message)
	}
	return out
}

// compactPromptZH 是压缩器的基础要求。
//
// 「用中文输出」那一条随语言换：摘要是 pinned 的、永不裁剪，一份语言不对的摘要会
// 一直留在上下文最前面，而且此后每次压缩都把它再抄一遍。
const compactPromptZH = `你是一个对话压缩器。请将下面的对话历史压缩成一份结构化摘要，要求：
- 保留：用户的核心诉求与目标、已确认的事实与结论、重要的工具调用及其关键结果、未完成的任务与待办事项
- 时间一律写成具体日期：原文里的「今天」「明天」「昨天」「下周」这类说法，按它所在的日界线（「（以下是 …… 的对话）」那一行）换算成日期再写，摘要里不要留下这些相对说法
- 未完成的待办要写清是哪一天说的、期限在哪一天；已经兑现或已经过期的，不要再写成待办
- 丢弃：寒暄客套、重复内容、已被后续纠正的失败中间过程
- 用中文输出，条理清晰、尽量精炼，直接输出摘要本身，不要额外说明
`

// compactExtrasHeader 引入插件追加的摘要要求。基础要求是按任务型对话写的（「寒暄客套」
// 被列为丢弃项），而角色扮演这类对话里关系性的内容恰恰长那个样子；追加项声明为优先，
// 与提示词注入「靠后的覆盖靠前的」是同一条约定。
const compactExtrasHeaderZH = "\n补充要求（与上面的要求冲突时以这里为准）：\n"

const compactHistoryHeaderZH = "\n对话历史：\n"

// buildCompactPrompt 拼出完整的压缩提示词：基础要求 + 插件追加的要求 + 历史正文。
func buildCompactPrompt(lang string, extras []string, history string) string {
	var b strings.Builder
	b.WriteString(prompt.T(lang, "prompt.core.compact", compactPromptZH))
	if len(extras) > 0 {
		b.WriteString(prompt.T(lang, "prompt.core.compact.extras", compactExtrasHeaderZH))
		for _, e := range extras {
			b.WriteString(e)
			b.WriteString("\n")
		}
	}
	b.WriteString(prompt.T(lang, "prompt.core.compact.history", compactHistoryHeaderZH))
	b.WriteString(history)
	return b.String()
}

// summaryPrefixZH 是摘要消息的开头，它会落盘、留在会话里。切换语言不重写已有的
// 摘要——那是当时的事实，改掉反而对不上摘要正文的语言。
const summaryPrefixZH = "以下是之前对话内容的压缩摘要：\n\n"

// summaryAnchorZH 是摘要末尾的时间锚。摘要是 pinned 的、永不参与裁剪，因此它里面的
// 时段描述会一直留在上下文里；不带锚的话，那些描述会被当成「现在」。
const summaryAnchorZH = "\n\n（以上对话发生在 %s 之前）"

// minSummarizeTokens 以下的分组不单独调模型生成摘要，原文原样保留。
// 一两轮对话的摘要信息量不会超过原文，却要付一次真实的模型调用。
const minSummarizeTokens = 1500

// groupSeparator 在流式播放多组摘要时插入，避免它们在界面上糊成一段。
const groupSeparator = "\n\n———\n\n"

// Compact 将 session 历史压缩为摘要消息。
// 摘要生成过程通过 emit 以 delta 事件流式发布，结束时发布 done 或 error。
// 与对话轮次共用轮次锁：压缩的 Replace 与并发轮次的 Append 竞态会丢消息。
func (a *Agent) Compact(ctx context.Context, sessionID string, emit func(Event)) {
	l := a.turnLock(sessionID)
	l.Lock()
	defer l.Unlock()
	provider, opts := a.snapshot()
	if err := a.compact(ctx, provider, opts, sessionID, emit); err != nil {
		emit(Event{Type: EventError, Error: err.Error()})
		return
	}
	emit(Event{Type: EventDone})
}

// CompactTurn 供插件以编程方式压缩会话（经 InitContext.Compact 暴露）。
// 与 RunTurn 相同的不排队语义：会话忙时立即返回 plugin.ErrSessionBusy。
func (a *Agent) CompactTurn(ctx context.Context, sessionID string) error {
	l := a.turnLock(sessionID)
	if !l.TryLock() {
		return plugin.ErrSessionBusy
	}
	defer l.Unlock()
	provider, opts := a.snapshot()
	return a.compact(ctx, provider, opts, sessionID, func(Event) {})
}

// scopeGroup 是历史中属于同一可见域的一段消息。
type scopeGroup struct {
	tag  string
	msgs []session.StoredMessage
	est  int // 该组的估算 token 量
}

// groupByTag 按可见域标签分组，组内保持原顺序。
//
// 组序按标签「最后一次出现」升序：当前活跃的可见域因此排在最后，压缩之后
// 「最后一条带标签消息」仍然指向它，依靠历史推导可见域状态的插件不会因压缩而反转。
func groupByTag(history []session.StoredMessage) []scopeGroup {
	idx := map[string]int{} // 标签 -> 在 groups 中的位置
	var groups []scopeGroup
	lastSeen := map[string]int{}
	for i, m := range history {
		g, ok := idx[m.Tag]
		if !ok {
			idx[m.Tag] = len(groups)
			g = len(groups)
			groups = append(groups, scopeGroup{tag: m.Tag})
		}
		groups[g].msgs = append(groups[g].msgs, m)
		if m.Kind != session.KindNotice {
			groups[g].est += estimateTokens(m.Message) // 注记不占上下文，不进压缩判据
		}
		lastSeen[m.Tag] = i
	}
	// 按最后出现位置排序
	for i := 1; i < len(groups); i++ {
		for j := i; j > 0 && lastSeen[groups[j-1].tag] > lastSeen[groups[j].tag]; j-- {
			groups[j-1], groups[j] = groups[j], groups[j-1]
		}
	}
	return groups
}

// largestGroup 返回估算 token 最多的分组下标。
func largestGroup(groups []scopeGroup) int {
	best := 0
	for i, g := range groups {
		if g.est > groups[best].est {
			best = i
		}
	}
	return best
}

// scopeForTag 构造「只读该标签与共享内容、写入该标签」的可见域，供压缩期间的插件回调
// 使用（决定归档落哪个目录、提炼出的记忆写哪个库）。这里是收紧的：提炼某个可见域的
// 记忆时，不该把别的可见域的内容混进去。
func scopeForTag(tag string) plugin.Scope {
	if tag == "" {
		return plugin.Scope{}
	}
	return plugin.Scope{Write: tag, Read: []string{tag}}
}

func (a *Agent) compact(ctx context.Context, provider llm.Provider, opts Options, sessionID string, emit func(Event)) error {
	_, history, err := a.store.Get(sessionID)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if len(history) == 0 {
		return fmt.Errorf("会话为空，无需压缩")
	}

	groups := groupByTag(history)
	// 无论体量如何，token 最多的那一组必须真的被压缩：否则上下文已超阈值却什么都没减少，
	// 自动压缩会在其后的每一轮空转重试，每次还白付一次模型调用。
	biggest := largestGroup(groups)

	out := make([]session.StoredMessage, 0, len(history))
	emitted := false
	for i, g := range groups {
		if i != biggest && g.est < minSummarizeTokens {
			// 原样保留（整组保留，tool 配对不受影响）。也不广播 OnCompact：
			// 原文没有丢失，不存在「信息即将消失」的时机。
			out = append(out, g.msgs...)
			continue
		}
		if emitted {
			emit(Event{Type: EventDelta, Content: groupSeparator})
		}
		// 本组的可见域进 ctx：插件据此决定归档落哪个目录、记忆写哪个库、摘要该守哪些要求
		gctx := plugin.WithScope(ctx, scopeForTag(g.tag))
		summary, err := a.summarize(ctx, provider, opts, g.msgs, a.plugins.CompactPrompts(gctx), emit)
		if err != nil {
			return err
		}
		emitted = true

		// 历史即将被物理删除，先广播给插件（可借此归档），其注记随摘要一并留在会话里
		notes := a.plugins.NotifyCompact(gctx, plugin.CompactEvent{
			SessionID: sessionID,
			Scope:     g.tag,
			History:   messagesOf(g.msgs),
			Summary:   summary,
		})
		// 摘要是 pinned 的、永不参与裁剪，因此它里面的时段描述会一直留在上下文里。
		// 不带时间锚的话，那些描述会被当成「现在」，压缩越多这种错位越顽固。
		now := time.Now()
		content := prompt.T(opts.Lang, "prompt.core.summary_prefix", summaryPrefixZH) + summary +
			prompt.Tf(opts.Lang, "prompt.core.summary_anchor", summaryAnchorZH, formatNow(opts.Lang, now))
		if len(notes) > 0 {
			content += "\n\n" + strings.Join(notes, "\n")
		}
		out = append(out, session.StoredMessage{
			Message: llm.Message{Role: llm.RoleUser, Content: content},
			Kind:    session.KindSummary,
			Tag:     g.tag,
			TS:      now,
		})
	}

	if err := a.store.Replace(sessionID, out); err != nil {
		return err
	}
	// 旧的实测用量对应压缩前的上下文，清除以免误导
	_ = a.store.SetUsage(sessionID, nil)
	return nil
}

// summarize 让模型把一组消息压成一段摘要，过程以 delta 事件流式发布。
// extras 是插件追加的摘要要求（见 plugin.CompactPrompter），可为空。
func (a *Agent) summarize(ctx context.Context, provider llm.Provider, opts Options, msgs []session.StoredMessage, extras []string, emit func(Event)) (string, error) {
	events, err := provider.ChatStream(ctx, llm.ChatRequest{
		Model:       opts.Model,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: buildCompactPrompt(opts.Lang, extras, serializeHistory(opts.Lang, msgs))}},
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Thinking:    "off", // 压缩追求速度，不启用思考
	})
	if err != nil {
		return "", err
	}
	var summary strings.Builder
	for ev := range events {
		switch ev.Type {
		case llm.EventContentDelta:
			summary.WriteString(ev.Content)
			emit(Event{Type: EventDelta, Content: ev.Content})
		case llm.EventError:
			return "", ev.Err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(summary.String()) == "" {
		return "", fmt.Errorf("摘要生成失败：模型返回为空")
	}
	return summary.String(), nil
}

// serializeHistory 把消息序列化为可读文本，工具结果做截断避免超长。
//
// 日期翻篇处插一行日界线。消息的时间戳本来就落了盘，此前一条都没交给压缩器——
// 它看到的是一堵没有任何时间信息的对话墙，于是原文里的「明天」只能原样抄进摘要。
// 而摘要是 pinned 的、永不裁剪，一条不带日期的待办就此永久留在上下文最前面，
// 每天以同样的措辞重新出现。要求摘要写绝对日期，前提是先把日期给它。
// 一次性输入（心跳提示词等）不进摘要：它们是机器注入的模板文案，不是对话内容。
func serializeHistory(lang string, msgs []session.StoredMessage) string {
	var b strings.Builder
	var day string
	for _, m := range msgs {
		if m.Kind == session.KindEphemeral || m.Kind == session.KindNotice {
			continue
		}
		if !m.TS.IsZero() {
			if d := m.TS.Format("2006-01-02"); d != day {
				day = d
				fmt.Fprintln(&b, dayMark(lang, m.TS))
			}
		}
		switch m.Role {
		case llm.RoleUser:
			fmt.Fprintf(&b, prompt.T(lang, "prompt.core.transcript.user", "用户: %s\n"), m.Content)
		case llm.RoleAssistant:
			if m.Content != "" {
				fmt.Fprintf(&b, prompt.T(lang, "prompt.core.transcript.assistant", "助手: %s\n"), m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, prompt.T(lang, "prompt.core.transcript.tool_call", "[助手调用工具 %s，参数 %s]\n"),
					tc.Name, truncateRunes(string(tc.Arguments), 200))
			}
		case llm.RoleTool:
			fmt.Fprintf(&b, prompt.T(lang, "prompt.core.transcript.tool_result", "[工具结果] %s\n"),
				truncateRunes(m.Content, 500))
		}
	}
	return b.String()
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…(截断)"
}
