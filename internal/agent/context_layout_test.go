package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"wen/internal/i18n"
	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// volatilePlugin 每轮给出不同的片段，模拟心情、天气、记忆索引这类会变的注入。
type volatilePlugin struct{ n int }

func (p *volatilePlugin) Name() string                                  { return "volatile" }
func (p *volatilePlugin) Description() string                           { return "测试用：每轮片段都不同" }
func (p *volatilePlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (p *volatilePlugin) Tools() []plugin.Tool                          { return nil }
func (p *volatilePlugin) SystemPrompt() string                          { return "[角色设定]\n固定不变的设定" }

func (p *volatilePlugin) TurnPrompt(context.Context, plugin.TurnEvent) (string, error) {
	p.n++
	return "[当前心情]\n第 " + string(rune('0'+p.n)) + " 轮", nil
}

// 提示词缓存的前提是 system 逐轮字节一致。每轮都变的片段一旦混进去，
// 前缀立刻作废——这条约束只靠约定维持，所以要有测试盯着。
func TestSystemPrefixIsIdenticalAcrossTurns(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	vp := &volatilePlugin{}
	provider := &mockProvider{turns: []mockTurn{{content: "答一"}, {content: "答二"}}}
	ag := New(provider, newTestManager(t, vp), store, Options{Model: "test", SystemPrompt: "用户配置"})

	ag.Run(context.Background(), meta.ID, "问一", func(Event) {})
	ag.Run(context.Background(), meta.ID, "问二", func(Event) {})

	first, second := provider.reqs[0].Messages[0], provider.reqs[1].Messages[0]
	if first.Role != llm.RoleSystem || second.Role != llm.RoleSystem {
		t.Fatal("首条应当是 system")
	}
	if first.Content != second.Content {
		t.Errorf("system 两轮之间必须逐字节一致，否则缓存前缀每轮作废：\n第一轮:\n%s\n第二轮:\n%s",
			first.Content, second.Content)
	}
	for _, unwanted := range []string{"当前时间", "[当前心情]"} {
		if strings.Contains(first.Content, unwanted) {
			t.Errorf("易变内容不该在 system 里: %q", unwanted)
		}
	}
	// 固定的插件片段与用户配置提示词仍在 system 里；工具约定同样是整轮不变的一段
	for _, want := range []string{"[系统环境]", "[工具与回复]", "[角色设定]", "用户配置"} {
		if !strings.Contains(first.Content, want) {
			t.Errorf("system 缺少 %q:\n%s", want, first.Content)
		}
	}
}

// 当前时间必须在上下文末尾：放在 system 开头时，模型会更采信历史里出现过的旧时刻。
func TestCurrentTimeRidesInLastMessage(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{{content: "答"}}}
	ag := New(provider, newTestManager(t, &volatilePlugin{}), store, Options{Model: "test"})

	ag.Run(context.Background(), meta.ID, "现在几点", func(Event) {})

	msgs := provider.reqs[0].Messages
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("最后一条应当是本轮用户消息，实际 %s", last.Role)
	}
	for _, want := range []string{"当前时间: ", "[当前心情]", "现在几点"} {
		if !strings.Contains(last.Content, want) {
			t.Errorf("末条缺少 %q:\n%s", want, last.Content)
		}
	}
	// 落盘的那条保持原样：本轮状态是发出去的副本上的东西，不属于对话内容
	_, stored, _ := store.Get(meta.ID)
	if stored[0].Content != "现在几点" {
		t.Errorf("落盘的用户消息被污染了: %q", stored[0].Content)
	}
}

// 隔了很久再开口时，历史里要有这段时间流逝的痕迹——否则上下文里那十几个小时不存在。
func TestLongGapIsMarkedInHistory(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewStore(dir)
	meta, _ := store.Create()
	old := time.Now().Add(-14 * time.Hour)
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleUser, Content: "昨晚的问"}, TS: old})
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleAssistant, Content: "昨晚的答"}, TS: old.Add(time.Minute)})
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleUser, Content: "今天的问"}, TS: time.Now()})

	provider := &mockProvider{turns: []mockTurn{{content: "答"}}}
	ag := New(provider, newTestManager(t), store, Options{Model: "test"})
	ag.Run(context.Background(), meta.ID, "接着说", func(Event) {})

	var marked string
	for _, m := range provider.reqs[0].Messages {
		if strings.Contains(m.Content, "此处距上一条消息") {
			marked = m.Content
		}
	}
	if !strings.Contains(marked, "今天的问") {
		t.Errorf("间隔标记应当落在隔了很久之后的那条用户消息上，实际 %q", marked)
	}
	if !strings.Contains(marked, "约 13 小时") && !strings.Contains(marked, "约 14 小时") {
		t.Errorf("间隔时长不对: %q", marked)
	}
	// 本轮与上一条之间几乎没有间隔，末条不该出现间隔行
	msgs := provider.reqs[0].Messages
	if strings.Contains(msgs[len(msgs)-1].Content, "距上一条消息: ") {
		t.Errorf("本轮不该标间隔: %q", msgs[len(msgs)-1].Content)
	}
	// 原文不受影响：标记只加在发出去的副本上
	_, stored, _ := store.Get(meta.ID)
	for _, m := range stored {
		if strings.Contains(m.Content, "此处距上一条消息") {
			t.Errorf("间隔标记不该落盘: %q", m.Content)
		}
	}
}

// dayMarkOf 从发出去的消息里挑出带某天日界线的那条。
func dayMarkOf(msgs []llm.Message, day string) (string, bool) {
	for _, m := range msgs {
		if strings.Contains(m.Content, "以下是 "+day) {
			return m.Content, true
		}
	}
	return "", false
}

// 历史里的「明天」要能被定位到具体哪一天，靠的是它写在哪条日界线下面。
// 只有 gapNote 的话，模型得把那条消息到末尾的间隔逐段累加才知道过了几天。
func TestDayBoundaryIsMarkedInHistory(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewStore(dir)
	meta, _ := store.Create()
	day1 := time.Now().Add(-48 * time.Hour)
	day2 := time.Now().Add(-24 * time.Hour)
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleUser, Content: "前天的问"}, TS: day1})
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleAssistant, Content: "前天的答"}, TS: day1.Add(time.Minute)})
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleUser, Content: "昨天的问"}, TS: day2})

	provider := &mockProvider{turns: []mockTurn{{content: "答"}}}
	ag := New(provider, newTestManager(t), store, Options{Model: "test"})
	ag.Run(context.Background(), meta.ID, "今天的问", func(Event) {})

	msgs := provider.reqs[0].Messages
	for _, d := range []string{day1.Format("2006-01-02"), day2.Format("2006-01-02")} {
		if _, ok := dayMarkOf(msgs, d); !ok {
			t.Errorf("缺 %s 的日界线:%s%+v", d, "\n", msgs)
		}
	}
	// 日界线落在那天第一条消息上，不是随便哪一条
	if got, _ := dayMarkOf(msgs, day2.Format("2006-01-02")); !strings.Contains(got, "昨天的问") {
		t.Errorf("日界线该落在当天第一条消息上，实际 %q", got)
	}
	// 同一天只标一次
	if n := strings.Count(joinContents(msgs), "以下是 "+day1.Format("2006-01-02")); n != 1 {
		t.Errorf("同一天应只标一次，得到 %d 次", n)
	}
	// 标注只加在发出去的副本上
	_, stored, _ := store.Get(meta.ID)
	for _, m := range stored {
		if strings.Contains(m.Content, "以下是 ") {
			t.Errorf("日界线不该落盘: %q", m.Content)
		}
	}
}

func joinContents(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// 角色主动开口的那一轮，注入的提示是一次性输入、后续上下文里已被剔掉，
// 那条助手消息前面没有用户消息可依附。此前它一点时间信号都没有——
// 而「昨天说的明天」多半正是那么说出口的。
func TestDayMarkStandsAloneBeforeAssistantMessage(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewStore(dir)
	meta, _ := store.Create()
	yesterday := time.Now().Add(-24 * time.Hour)
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleUser, Content: "前天聊的"}, TS: yesterday.Add(-24 * time.Hour)})
	// 心跳那一轮：一次性输入不进后续上下文，只剩助手自己说的这句
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleUser, Content: "【心跳】"}, TS: yesterday, Kind: session.KindEphemeral})
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleAssistant, Content: "明天给你带菜"}, TS: yesterday.Add(time.Minute)})

	provider := &mockProvider{turns: []mockTurn{{content: "答"}}}
	ag := New(provider, newTestManager(t), store, Options{Model: "test"})
	ag.Run(context.Background(), meta.ID, "今天的问", func(Event) {})

	msgs := provider.reqs[0].Messages
	var idx = -1
	for i, m := range msgs {
		if m.Content == "明天给你带菜" {
			idx = i
		}
	}
	if idx < 1 {
		t.Fatalf("没找到那条助手消息:%s%+v", "\n", msgs)
	}
	prev := msgs[idx-1]
	if !strings.Contains(prev.Content, "以下是 "+yesterday.Format("2006-01-02")) {
		t.Errorf("助手主动开口那条前面应单独插一条日界线，实际前一条是 %q", prev.Content)
	}
	if prev.Role != llm.RoleUser {
		t.Errorf("插入的标注应是 user 角色（组装层会与相邻的 user 合并），实际 %q", prev.Role)
	}
}

// 工具结果那几条中间插不得：Anthropic 侧连续的 user 消息会合并成一条，
// tool_result block 必须排在最前，中间夹一段文字就把它们拆散了。
// 跨零点的轮次因此要把日界线顺延到工具结果之后，而不是插在中间。
func TestDayMarkNeverSplitsToolResults(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewStore(dir)
	meta, _ := store.Create()
	// 真正跨零点：昨天 23:50 发起，两个结果分别落在 23:55 与今天 00:05
	n := time.Now()
	mid := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.Local)
	before := mid.Add(-10 * time.Minute)
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleUser, Content: "查一下"}, TS: before})
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "echo", Arguments: []byte("{}")},
			{ID: "c2", Name: "echo", Arguments: []byte("{}")},
		}}, TS: before})
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleTool, Content: "结果一", ToolCallID: "c1"},
		TS:      before.Add(5 * time.Minute)})
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleTool, Content: "结果二", ToolCallID: "c2"},
		TS:      mid.Add(5 * time.Minute)})
	store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleAssistant, Content: "查到了"}, TS: mid.Add(5 * time.Minute)})

	provider := &mockProvider{turns: []mockTurn{{content: "答"}}}
	ag := New(provider, newTestManager(t), store, Options{Model: "test"})
	ag.Run(context.Background(), meta.ID, "今天的问", func(Event) {})

	msgs := provider.reqs[0].Messages
	for i, m := range msgs {
		if len(m.ToolCalls) > 0 && (i+1 >= len(msgs) || msgs[i+1].Role != llm.RoleTool) {
			t.Fatalf("tool_use 之后必须紧跟 tool_result，实际第 %d 条之后是 %+v", i, msgs[i+1:])
		}
		// 两条工具结果之间不能夹任何东西
		if m.Role == llm.RoleTool && m.ToolCallID == "c1" {
			if i+1 >= len(msgs) || msgs[i+1].Role != llm.RoleTool {
				t.Fatalf("日界线被插进了两条工具结果之间: %+v", msgs[i+1:])
			}
		}
	}
	// 顺延而不是丢掉：跨过零点的那一天仍要标出来，落点在工具结果之后
	if _, ok := dayMarkOf(msgs, mid.Format("2006-01-02")); !ok {
		t.Fatalf("被工具结果挡住的日界线应顺延，而不是丢掉:%s%+v", "\n", msgs)
	}
	for i, m := range msgs {
		if m.Content == "查到了" && (i < 1 || !strings.Contains(msgs[i-1].Content, "以下是 "+mid.Format("2006-01-02"))) {
			t.Errorf("顺延后的日界线该落在工具结果之后那条助手消息前，实际前一条是 %+v", msgs[i-1])
		}
	}
}

// [历史与时间] 里引述了两种标注的措辞，而真正生成它们的是 dayMark 与 gapNote。
// 两处分头改就会指向一段模型在上下文里根本找不到的东西，说明反而变成误导。
//
// **逐语言跑**：日界线与历史规则在每门语言里各有一份，只对上中文那份不算数——
// 一个英文用户拿到的会是「说明里引述的措辞在上下文里根本不存在」。
func TestHistoryRulesQuoteRealMarkers(t *testing.T) {
	for _, l := range i18n.Languages() {
		rules := historyRulesFor(l.Code)
		for _, tc := range []struct{ what, format string }{
			{"日界线", dayMarkFmt(l.Code)},
			{"间隔", gapNoteFmt(l.Code)},
		} {
			got := longestLiteral(tc.format)
			if got == "" || !strings.Contains(rules, got) {
				t.Errorf("%s: [历史与时间] 没有引用真正的%s措辞 %q:\n%s", l.Code, tc.what, got, rules)
			}
		}
	}
}

// longestLiteral 取格式串里最长的一段固定措辞：按占位符切开，取最长的那块。
//
// 比「截到第一个数字为止」通用——后者是照着中文的语序写的，英文里日期不一定在最后，
// 固定措辞也不一定在数字前面。
func longestLiteral(format string) string {
	best := ""
	for i, part := range strings.Split(format, "%") {
		// 除第一段外，每段开头那个字符是占位符的类型（s / d），不是措辞
		if i > 0 {
			if len(part) < 2 {
				continue
			}
			part = part[1:]
		}
		if part = strings.Trim(part, "\n"); len(part) > len(best) {
			best = part
		}
	}
	return best
}

// 这段是缓存前缀的一部分，必须整轮之间逐字节不变——它是常量，测试盯着别人把它改成函数。
func TestHistoryRulesIsInSystem(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewStore(dir)
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{{content: "答"}, {content: "答"}}}
	ag := New(provider, newTestManager(t), store, Options{Model: "test"})
	ag.Run(context.Background(), meta.ID, "一", func(Event) {})
	ag.Run(context.Background(), meta.ID, "二", func(Event) {})
	for i, r := range provider.reqs {
		if !strings.Contains(r.Messages[0].Content, "[历史与时间]") {
			t.Fatalf("第 %d 轮的 system 里没有 [历史与时间]", i)
		}
	}
	if provider.reqs[0].Messages[0].Content != provider.reqs[1].Messages[0].Content {
		t.Error("system 前缀在两轮之间发生了变化")
	}
}
