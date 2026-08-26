package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// compactRecorder 记录每次收到的压缩事件，用于验证按域广播。
type compactRecorder struct {
	name string
	got  []plugin.CompactEvent
}

func (p *compactRecorder) Name() string                                  { return p.name }
func (p *compactRecorder) Description() string                           { return "记录压缩事件" }
func (p *compactRecorder) Init(plugin.InitContext, map[string]any) error { return nil }
func (p *compactRecorder) SystemPrompt() string                          { return "" }
func (p *compactRecorder) Tools() []plugin.Tool                          { return nil }

func (p *compactRecorder) OnCompact(ctx context.Context, ev plugin.CompactEvent) (string, error) {
	p.got = append(p.got, ev)
	// 顺带验证核心把该组的可见域放进了 ctx：归档与记忆分库都靠它
	if sc := plugin.ScopeFrom(ctx); sc.Write != ev.Scope {
		return "", nil
	}
	return "（已归档 " + ev.Scope + "）", nil
}

// bulk 造一段足够大的消息，确保分组超过 minSummarizeTokens。
func bulk(role, tag string) session.StoredMessage {
	return tagged(msg(role, strings.Repeat("字", minSummarizeTokens*3+300)), tag)
}

func TestGroupByTagOrdersByLastOccurrence(t *testing.T) {
	history := []session.StoredMessage{
		tagged(msg(llm.RoleUser, "b1"), "b"),
		tagged(msg(llm.RoleUser, "a1"), "a"),
		tagged(msg(llm.RoleUser, "b2"), "b"),
		tagged(msg(llm.RoleUser, "a2"), "a"),
	}
	groups := groupByTag(history)
	if len(groups) != 2 {
		t.Fatalf("应分成 2 组，得到 %d", len(groups))
	}
	// 按「最后一次出现」排序：a 最后出现在 3，b 在 2，所以 a 在后
	if groups[0].tag != "b" || groups[1].tag != "a" {
		t.Errorf("组序 = %q,%q，want b,a", groups[0].tag, groups[1].tag)
	}
	// 组内保持原顺序
	if groups[1].msgs[0].Content != "a1" || groups[1].msgs[1].Content != "a2" {
		t.Errorf("组内顺序被打乱: %+v", groups[1].msgs)
	}
}

func TestCompactSplitsByScope(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	for _, m := range []session.StoredMessage{
		bulk(llm.RoleUser, "a"), bulk(llm.RoleAssistant, "a"),
		bulk(llm.RoleUser, "b"), bulk(llm.RoleAssistant, "b"),
	} {
		if err := store.Append(meta.ID, m); err != nil {
			t.Fatal(err)
		}
	}

	rec := &compactRecorder{name: "recorder"}
	provider := &mockProvider{turns: []mockTurn{{content: "摘要甲"}, {content: "摘要乙"}}}
	ag := New(provider, newTestManager(t, rec), store, Options{Model: "test"})

	var delta strings.Builder
	ag.Compact(context.Background(), meta.ID, func(ev Event) {
		if ev.Type == EventDelta {
			delta.WriteString(ev.Content)
		}
	})

	_, msgs, _ := store.Get(meta.ID)
	if len(msgs) != 2 {
		t.Fatalf("应留下两条摘要，得到 %d 条: %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.Kind != session.KindSummary {
			t.Errorf("应是摘要消息: %+v", m)
		}
	}
	if msgs[0].Tag != "a" || msgs[1].Tag != "b" {
		t.Errorf("摘要应带各自的可见域标签: %q,%q", msgs[0].Tag, msgs[1].Tag)
	}
	// 每组各广播一次，且带上本组的标签
	if len(rec.got) != 2 || rec.got[0].Scope != "a" || rec.got[1].Scope != "b" {
		t.Fatalf("按域广播不对: %+v", rec.got)
	}
	// 广播的历史只含本组消息，不能把别的域混进去
	if len(rec.got[0].History) != 2 {
		t.Errorf("每组广播应只含本组的 2 条消息，得到 %d", len(rec.got[0].History))
	}
	// 注记落进对应那条摘要
	if !strings.Contains(msgs[0].Content, "已归档 a") || !strings.Contains(msgs[1].Content, "已归档 b") {
		t.Error("插件注记应追加到各自的摘要末尾")
	}
	// 多组摘要在界面上需要分隔，否则糊成一段
	if !strings.Contains(delta.String(), groupSeparator) {
		t.Error("多组摘要之间应发出分隔符")
	}
}

func TestCompactKeepsSmallGroupVerbatim(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	for _, m := range []session.StoredMessage{
		bulk(llm.RoleUser, "big"), bulk(llm.RoleAssistant, "big"),
		tagged(msg(llm.RoleUser, "一句话"), "small"),
	} {
		store.Append(meta.ID, m)
	}

	rec := &compactRecorder{name: "recorder"}
	provider := &mockProvider{turns: []mockTurn{{content: "摘要"}}}
	ag := New(provider, newTestManager(t, rec), store, Options{Model: "test"})
	ag.Compact(context.Background(), meta.ID, func(Event) {})

	if provider.calls != 1 {
		t.Errorf("小分组不该单独调模型，共调用 %d 次", provider.calls)
	}
	_, msgs, _ := store.Get(meta.ID)
	if len(msgs) != 2 {
		t.Fatalf("应是 1 条摘要 + 1 条原文，得到 %d 条", len(msgs))
	}
	// 组序按最后出现，small 在后
	if msgs[0].Kind != session.KindSummary || msgs[0].Tag != "big" {
		t.Errorf("第一条应是 big 的摘要: %+v", msgs[0])
	}
	if msgs[1].Kind != "" || msgs[1].Content != "一句话" {
		t.Errorf("小分组应原文保留: %+v", msgs[1])
	}
	// 原文没有丢失，就不该触发归档/提炼
	if len(rec.got) != 1 || rec.got[0].Scope != "big" {
		t.Errorf("小分组不该广播压缩事件: %+v", rec.got)
	}
}

func TestCompactAlwaysSummarizesLargestGroup(t *testing.T) {
	// 每组都小于阈值时也必须压掉最大的一组，否则自动压缩会在阈值以上反复空转
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	store.Append(meta.ID, tagged(msg(llm.RoleUser, strings.Repeat("字", 300)), "a"))
	store.Append(meta.ID, tagged(msg(llm.RoleUser, "短"), "b"))

	provider := &mockProvider{turns: []mockTurn{{content: "摘要"}}}
	ag := New(provider, newTestManager(t), store, Options{Model: "test"})
	ag.Compact(context.Background(), meta.ID, func(Event) {})

	if provider.calls != 1 {
		t.Fatalf("应恰好压缩一组，调用 %d 次", provider.calls)
	}
	_, msgs, _ := store.Get(meta.ID)
	var summarized []string
	for _, m := range msgs {
		if m.Kind == session.KindSummary {
			summarized = append(summarized, m.Tag)
		}
	}
	if len(summarized) != 1 || summarized[0] != "a" {
		t.Errorf("应压缩体量最大的 a 组，得到 %v", summarized)
	}
}

func TestCompactUntaggedHistoryUnchanged(t *testing.T) {
	// 回归：升级前的会话历史全无标签，压缩行为必须与改动前一致——一条无标签摘要
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	store.Append(meta.ID, msg(llm.RoleUser, "问"))
	store.Append(meta.ID, msg(llm.RoleAssistant, "答"))

	rec := &compactRecorder{name: "recorder"}
	provider := &mockProvider{turns: []mockTurn{{content: "摘要正文"}}}
	ag := New(provider, newTestManager(t, rec), store, Options{Model: "test"})

	var delta strings.Builder
	ag.Compact(context.Background(), meta.ID, func(ev Event) {
		if ev.Type == EventDelta {
			delta.WriteString(ev.Content)
		}
	})

	_, msgs, _ := store.Get(meta.ID)
	if len(msgs) != 1 || msgs[0].Kind != session.KindSummary || msgs[0].Tag != "" {
		t.Fatalf("应留下一条无标签摘要: %+v", msgs)
	}
	if !strings.HasPrefix(msgs[0].Content, summaryPrefixZH) {
		t.Errorf("摘要前缀丢了: %q", msgs[0].Content)
	}
	if len(rec.got) != 1 || rec.got[0].Scope != "" {
		t.Errorf("无标签历史广播的 Scope 应为空: %+v", rec.got)
	}
	if strings.Contains(delta.String(), groupSeparator) {
		t.Error("只有一组时不该出现分隔符")
	}
	if usage := func() *session.Usage { m, _, _ := store.Get(meta.ID); return m.LastUsage }(); usage != nil {
		t.Error("压缩后应清除旧的实测用量")
	}
}

func TestCompactEmptySession(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	ag := New(&mockProvider{}, newTestManager(t), store, Options{Model: "test"})

	var gotErr string
	ag.Compact(context.Background(), meta.ID, func(ev Event) {
		if ev.Type == EventError {
			gotErr = ev.Error
		}
	})
	if gotErr == "" {
		t.Error("空会话应报错而不是静默成功")
	}
}

// compactPrompterStub 给压缩提示词追加一段要求，并记下每次收到的可见域。
type compactPrompterStub struct {
	name   string
	frag   string
	scopes []string
}

func (p *compactPrompterStub) Name() string                                  { return p.name }
func (p *compactPrompterStub) Description() string                           { return "追加压缩要求" }
func (p *compactPrompterStub) Init(plugin.InitContext, map[string]any) error { return nil }
func (p *compactPrompterStub) SystemPrompt() string                          { return "" }
func (p *compactPrompterStub) Tools() []plugin.Tool                          { return nil }
func (p *compactPrompterStub) CompactPrompt(ctx context.Context) string {
	p.scopes = append(p.scopes, plugin.ScopeFrom(ctx).Write)
	return p.frag
}

func TestCompactPromptCarriesPluginRequirements(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	for _, m := range []session.StoredMessage{bulk(llm.RoleUser, "a"), bulk(llm.RoleAssistant, "a")} {
		if err := store.Append(meta.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	stub := &compactPrompterStub{name: "stub", frag: "- 保留关系状态"}
	provider := &mockProvider{turns: []mockTurn{{content: "摘要"}}}
	ag := New(provider, newTestManager(t, stub), store, Options{Model: "test"})
	ag.Compact(context.Background(), meta.ID, func(Event) {})

	if len(provider.reqs) != 1 {
		t.Fatalf("应只有一次摘要请求，得到 %d", len(provider.reqs))
	}
	prompt := provider.reqs[0].Messages[0].Content
	if !strings.Contains(prompt, compactExtrasHeaderZH+"- 保留关系状态\n") {
		t.Errorf("插件的要求应随补充段进入提示词:\n%s", truncateRunes("", prompt, 400))
	}
	// 补充段在基础要求之后、历史正文之前：它要盖过基础要求，又不能混进对话内容
	if strings.Index(prompt, compactExtrasHeaderZH) > strings.Index(prompt, compactHistoryHeaderZH) {
		t.Error("补充段应位于历史正文之前")
	}
	if len(stub.scopes) != 1 || stub.scopes[0] != "a" {
		t.Errorf("本组的可见域应随 ctx 交给插件: %v", stub.scopes)
	}
}

func TestCompactPromptPlainWithoutPrompters(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	for _, m := range []session.StoredMessage{bulk(llm.RoleUser, ""), bulk(llm.RoleAssistant, "")} {
		store.Append(meta.ID, m)
	}
	provider := &mockProvider{turns: []mockTurn{{content: "摘要"}}}
	ag := New(provider, newTestManager(t, &compactRecorder{name: "r"}), store, Options{Model: "test"})
	ag.Compact(context.Background(), meta.ID, func(Event) {})

	prompt := provider.reqs[0].Messages[0].Content
	if strings.Contains(prompt, compactExtrasHeaderZH) {
		t.Error("没有插件追加要求时不该出现补充段")
	}
	if !strings.HasPrefix(prompt, compactPromptZH) || !strings.Contains(prompt, compactHistoryHeaderZH) {
		t.Error("基础要求与历史正文应原样保留")
	}
}

// at 造一个本地时刻，日期部分是这几个用例关心的全部。
func at(y, mo, d, hh, mm int) time.Time {
	return time.Date(y, time.Month(mo), d, hh, mm, 0, 0, time.Local)
}

func stamped(m session.StoredMessage, ts time.Time) session.StoredMessage {
	m.TS = ts
	return m
}

// 压缩器此前拿到的是一堵没有时间信息的对话墙，原文里的「明天」只能原样抄进摘要，
// 而摘要永不裁剪。日界线是它写出绝对日期的唯一原料。
func TestSerializeHistoryMarksDayBoundaries(t *testing.T) {
	got := serializeHistory("", []session.StoredMessage{
		stamped(msg(llm.RoleUser, "u1"), at(2026, 8, 20, 9, 0)),
		stamped(msg(llm.RoleAssistant, "明天给你带菜"), at(2026, 8, 20, 22, 0)),
		stamped(msg(llm.RoleUser, "u2"), at(2026, 8, 21, 8, 0)),
	})
	want := "（以下是 2026-08-20 星期四 的对话）\n用户: u1\n助手: 明天给你带菜\n" +
		"（以下是 2026-08-21 星期五 的对话）\n用户: u2\n"
	if got != want {
		t.Errorf("序列化结果不符:\n%s\nwant:\n%s", got, want)
	}
}

// 同一天只标一次，且没有时间戳的消息（旧会话）不该凭空造出日界线。
func TestSerializeHistoryOneMarkPerDay(t *testing.T) {
	got := serializeHistory("", []session.StoredMessage{
		stamped(msg(llm.RoleUser, "a"), at(2026, 8, 20, 9, 0)),
		stamped(msg(llm.RoleUser, "b"), at(2026, 8, 20, 18, 0)),
		msg(llm.RoleUser, "c"),
	})
	if n := strings.Count(got, "以下是 2026-08-20"); n != 1 {
		t.Errorf("同一天应只标一次，得到 %d 次:\n%s", n, got)
	}
	if strings.Count(got, "以下是") != 1 {
		t.Errorf("缺时间戳的消息不该产生日界线:\n%s", got)
	}
}

// 一次性输入与注记不进压缩原文，它们的日期也不该带出日界线来。
func TestSerializeHistorySkipsEphemeralForDayMark(t *testing.T) {
	eph := stamped(msg(llm.RoleUser, "心跳提示"), at(2026, 8, 21, 3, 0))
	eph.Kind = session.KindEphemeral
	got := serializeHistory("", []session.StoredMessage{
		stamped(msg(llm.RoleUser, "u1"), at(2026, 8, 20, 9, 0)),
		eph,
		stamped(msg(llm.RoleUser, "u2"), at(2026, 8, 21, 8, 0)),
	})
	if strings.Contains(got, "心跳提示") {
		t.Errorf("一次性输入不该进压缩原文:\n%s", got)
	}
	if !strings.Contains(got, "（以下是 2026-08-21 星期五 的对话）\n用户: u2") {
		t.Errorf("日界线应落在真正进入原文的那条消息前:\n%s", got)
	}
}

// 摘要永不裁剪，里面留下「明天」就等于把一条空气待办焊死在上下文最前面。
func TestCompactPromptDemandsAbsoluteDates(t *testing.T) {
	p := buildCompactPrompt("", nil, "用户: x")
	for _, want := range []string{"具体日期", "「明天」", "期限在哪一天"} {
		if !strings.Contains(p, want) {
			t.Errorf("压缩提示词缺少对绝对日期的要求 %q:\n%s", want, p)
		}
	}
}
