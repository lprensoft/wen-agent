package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

// ---------- 切词 ----------

func TestTokenizeCJKBigrams(t *testing.T) {
	got := tokenize("我养的猫叫小白")
	for _, want := range []string{"我养", "养的", "的猫", "猫叫", "叫小", "小白"} {
		if !got[want] {
			t.Errorf("应切出二元组 %q，实际 %v", want, got)
		}
	}
	if got["我"] || got["白"] {
		t.Error("单个中文字不该成为 token")
	}
}

func TestTokenizeWordsLowercased(t *testing.T) {
	got := tokenize("Use Go 1.22 with PostgreSQL")
	for _, want := range []string{"use", "go", "22", "postgresql"} {
		if !got[want] {
			t.Errorf("应切出单词 %q，实际 %v", want, got)
		}
	}
	if got["Go"] || got["PostgreSQL"] {
		t.Error("英文单词应统一小写")
	}
	if got["1"] {
		t.Error("单字符的单词不该成为 token")
	}
}

func TestTokenizeDropsStopTokens(t *testing.T) {
	got := tokenize("我们可以一下什么 the and")
	for _, bad := range []string{"我们", "可以", "一下", "什么", "the", "and"} {
		if got[bad] {
			t.Errorf("虚词 %q 不该参与打分", bad)
		}
	}
}

func TestTokenizeMixedScripts(t *testing.T) {
	got := tokenize("周末去了Costco买牛排")
	for _, want := range []string{"周末", "末去", "去了", "costco", "买牛", "牛排"} {
		if !got[want] {
			t.Errorf("混排应切出 %q，实际 %v", want, got)
		}
	}
	// 英文单词把中文串断开：不该出现跨语言的二元组
	if got["了c"] || got["o买"] {
		t.Error("中英边界不该拼成二元组")
	}
}

// ---------- 打分与阈值 ----------

func entryAt(name, desc, content string, updated time.Time) Entry {
	return Entry{Name: name, Description: desc, Type: "事实", Content: content, Updated: updated}
}

func TestScoreTitleHitOutweighsContent(t *testing.T) {
	query := tokenize("你还记得小白那只猫现在怎么样了吗")
	byTitle := scoreEntry(query, entryAt("小白那只猫", "家里的猫", "一只橘猫。", time.Now()))
	byContent := scoreEntry(query, entryAt("宠物", "养过的动物", "小白那只猫很黏人。", time.Now()))
	if byTitle.titleHits == 0 || byContent.titleHits != 0 {
		t.Fatalf("命中位置判断有误：%+v / %+v", byTitle, byContent)
	}
	if byTitle.raw <= byContent.raw {
		t.Errorf("命中标题的加权分（%d）应高于只命中正文的（%d）", byTitle.raw, byContent.raw)
	}
}

func TestPassesThresholds(t *testing.T) {
	cases := []struct {
		name string
		hit  recallHit
		want bool
	}{
		{"标题命中一个但总分不够", recallHit{titleHits: 1, raw: 3}, false},
		{"标题命中一个加正文一个", recallHit{titleHits: 1, contentHits: 1, raw: 4}, true},
		{"正文命中三个不够", recallHit{contentHits: 3, raw: 3}, false},
		{"正文独自命中四个", recallHit{contentHits: 4, raw: 4}, true},
		{"什么都没命中", recallHit{}, false},
	}
	for _, c := range cases {
		if got := c.hit.passes(4); got != c.want {
			t.Errorf("%s：passes = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPickRecallsSuppressesLongEntries(t *testing.T) {
	now := time.Now()
	short := entryAt("小白那只猫", "家里的猫", "一只橘猫。", now)
	long := entryAt("小白那只猫的一年", "家里的猫",
		strings.Repeat("它早上会跳到床头叫人起床，中午趴在窗台晒太阳，晚上钻进被窝。", 12), now)
	hits := pickRecalls("你还记得小白那只猫现在怎么样了吗", []Entry{long, short}, 5, 4)
	if len(hits) != 2 {
		t.Fatalf("两条都该够格，实际 %d 条", len(hits))
	}
	if hits[0].entry.Name != short.Name {
		t.Errorf("同等命中下短条目应排在前面，实际第一条是 %q", hits[0].entry.Name)
	}
}

func TestPickRecallsTieBreaksByUpdated(t *testing.T) {
	now := time.Now()
	// 两条文本完全相同（pickRecalls 是纯函数，不关心同名），只差更新时间
	older := entryAt("小白那只猫", "家里的猫", "一只橘猫。", now.Add(-48*time.Hour))
	newer := entryAt("小白那只猫", "家里的猫", "一只橘猫。", now)
	hits := pickRecalls("你还记得小白那只猫现在怎么样了吗", []Entry{older, newer}, 5, 4)
	if len(hits) != 2 {
		t.Fatalf("两条都该够格，实际 %d 条", len(hits))
	}
	if hits[0].score != hits[1].score {
		t.Fatalf("两条分数应相同：%v / %v", hits[0].score, hits[1].score)
	}
	if !hits[0].entry.Updated.Equal(newer.Updated) {
		t.Errorf("同分应按更新时间新的优先，实际第一条更新于 %v", hits[0].entry.Updated)
	}
}

func TestPickRecallsIgnoresShortInput(t *testing.T) {
	e := entryAt("好的", "好的好的", "好的好的好的", time.Now())
	if hits := pickRecalls("好的", []Entry{e}, 5, 1); len(hits) != 0 {
		t.Errorf("「好的」这种短输入不该触发召回，实际召回 %d 条", len(hits))
	}
	if hits := pickRecalls("嗯", []Entry{e}, 5, 1); len(hits) != 0 {
		t.Errorf("「嗯」不该触发召回，实际召回 %d 条", len(hits))
	}
}

func TestPickRecallsRespectsMax(t *testing.T) {
	now := time.Now()
	var entries []Entry
	for _, n := range []string{"小白那只猫一", "小白那只猫二", "小白那只猫三"} {
		entries = append(entries, entryAt(n, "家里的猫", "一只橘猫。", now))
	}
	if hits := pickRecalls("你还记得小白那只猫现在怎么样了吗", entries, 2, 4); len(hits) != 2 {
		t.Errorf("最多召回 2 条，实际 %d 条", len(hits))
	}
	if hits := pickRecalls("你还记得小白那只猫现在怎么样了吗", entries, 0, 4); len(hits) != 0 {
		t.Errorf("上限为 0 表示关闭，实际召回 %d 条", len(hits))
	}
}

// ---------- 渲染与预算 ----------

func TestRenderRecallsTruncatesBody(t *testing.T) {
	e := entryAt("小白那只猫", "家里的猫", strings.Repeat("猫", 500), time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC))
	block, n := renderRecalls("", []recallHit{{entry: e}}, 100, 0)
	if n != 1 {
		t.Fatalf("应放进 1 条，实际 %d", n)
	}
	if !strings.HasPrefix(block, recallHeaderZH) {
		t.Errorf("块应以 %q 开头", recallHeaderZH)
	}
	if !strings.Contains(block, "「事实/小白那只猫」（更新于 2026-05-10）") {
		t.Errorf("应注明来源标题与更新日期:\n%s", block)
	}
	if !strings.Contains(block, recallEllipsisZH) {
		t.Error("超长正文应被截断并注明后文略")
	}
	if strings.Count(block, "猫") > 100+2 { // 标题里还有一个「猫」
		t.Errorf("正文应截到 100 字，实际块里有 %d 个「猫」", strings.Count(block, "猫"))
	}
}

func TestRenderRecallsHonorsByteBudget(t *testing.T) {
	now := time.Now()
	hits := []recallHit{
		{entry: entryAt("第一条", "摘要", strings.Repeat("内容", 200), now)},
		{entry: entryAt("第二条", "摘要", strings.Repeat("内容", 200), now)},
	}
	block, n := renderRecalls("", hits, 0, 700)
	if n != 1 {
		t.Errorf("700 字节只装得下一条，实际放进 %d 条", n)
	}
	if len(block) > 700 {
		t.Errorf("块超出预算：%d 字节", len(block))
	}
	if !strings.Contains(block, "第一条") || strings.Contains(block, "第二条") {
		t.Error("预算不够时应保留排序靠前的那条")
	}
	// 连一条都装不下时整块不出现，而不是只剩一个标题
	if block, n := renderRecalls("", hits, 0, 60); n != 0 || block != "" {
		t.Errorf("一条都装不下时块应为空，实际 n=%d block=%q", n, block)
	}
}

// ---------- 注入 ----------

// turnPrompt 以给定输入与可见域取一次本轮注入。
func turnPrompt(t *testing.T, p *Plugin, ctx context.Context, input string) string {
	t.Helper()
	s, err := p.TurnPrompt(ctx, plugin.TurnEvent{UserInput: input})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func saveCat(t *testing.T, p *Plugin, sc plugin.Scope) {
	t.Helper()
	store := p.storeFor(sc.Write)
	_, err := store.Save(Entry{
		Name:        "小白那只猫",
		Description: "家里养的橘猫",
		Type:        "事实",
		Content:     "小白是三年前从朋友家抱回来的橘猫，" + strings.Repeat("喜欢趴在窗台晒太阳。", 60),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
}

const catQuestion = "你还记得小白那只猫现在怎么样了吗，它还趴窗台晒太阳吗"

func TestTurnPromptRecallsBeforeIndex(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"turn_extract": false, "recall_entry_runes": 100})
	saveCat(t, p, plugin.Scope{})

	got := turnPrompt(t, p, context.Background(), catQuestion)
	ri, ii := strings.Index(got, recallHeaderZH), strings.Index(got, "[已保存的记忆]")
	if ri < 0 {
		t.Fatalf("应注入 [想起来的事]:\n%s", got)
	}
	if ii < 0 || ri > ii {
		t.Errorf("召回块应排在索引块之前（召回 %d、索引 %d）", ri, ii)
	}
	if !strings.Contains(got, "三年前从朋友家抱回来") {
		t.Error("召回块应带正文")
	}
	if !strings.Contains(got, recallEllipsisZH) {
		t.Error("正文应按配置的字数截断")
	}
	// 索引里照样列着：索引是目录，召回是内容
	if !strings.Contains(got[ii:], "事实/小白那只猫") {
		t.Error("被召回的条目在索引里仍应保留")
	}
}

func TestTurnPromptNoRecallForUnrelatedInput(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"turn_extract": false})
	saveCat(t, p, plugin.Scope{})

	got := turnPrompt(t, p, context.Background(), "明天的会议材料我已经发到邮箱里了，记得看一眼")
	if strings.Contains(got, recallHeaderZH) {
		t.Errorf("无关的话不该召回:\n%s", got)
	}
	if !strings.Contains(got, "[已保存的记忆]") {
		t.Error("索引仍应注入")
	}
}

func TestTurnPromptRecallCanBeDisabled(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"turn_extract": false, "recall_max": 0})
	saveCat(t, p, plugin.Scope{})
	if got := turnPrompt(t, p, context.Background(), catQuestion); strings.Contains(got, recallHeaderZH) {
		t.Error("recall_max 为 0 时不该召回")
	}
}

func TestTurnPromptSkipsRecallForEphemeralInput(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"turn_extract": false})
	saveCat(t, p, plugin.Scope{})
	ctx := plugin.WithEphemeralInput(context.Background())
	if got := turnPrompt(t, p, ctx, catQuestion); strings.Contains(got, recallHeaderZH) {
		t.Error("机器注入的一次性输入不该触发召回")
	}
}

func TestTurnPromptRecallTouchesEntry(t *testing.T) {
	p := newTestPlugin(t, decayCfg(30, 90))
	saveCat(t, p, plugin.Scope{})
	store := p.storeFor("")
	backdate(t, store, "小白那只猫", 20)

	turnPrompt(t, p, context.Background(), catQuestion)
	e, err := store.Get("小白那只猫")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(e.LastUsed) > time.Minute {
		t.Errorf("被召回的记忆应刷新最后使用时间，实际 %v", e.LastUsed)
	}
}

func TestTurnPromptRecallNoTouchWhenDecayOff(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"turn_extract": false, "decay": false})
	saveCat(t, p, plugin.Scope{})
	store := p.storeFor("")
	backdate(t, store, "小白那只猫", 20)

	turnPrompt(t, p, context.Background(), catQuestion)
	e, err := store.Get("小白那只猫")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(e.LastUsed) < 24*time.Hour {
		t.Error("没开淡忘时没人读最后使用时间，不该为它写盘")
	}
}

// 不可读域的记忆不会被召回——这是泄漏点：索引按域过滤了，召回要是绕过去，
// 正文就从这里漏出来了。
func TestTurnPromptRecallRespectsScope(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"turn_extract": false})
	saveCat(t, p, innerScope) // 写进 inner 库

	if got := turnPrompt(t, p, scoped(outerScope), catQuestion); strings.Contains(got, "小白") {
		t.Errorf("outer 域不该看到 inner 域的记忆:\n%s", got)
	}
	if got := turnPrompt(t, p, scoped(innerScope), catQuestion); !strings.Contains(got, recallHeaderZH) {
		t.Errorf("inner 域应能召回自己的记忆:\n%s", got)
	}
}

func TestGuideExplainsRecallBlock(t *testing.T) {
	got := newTestPlugin(t, nil).SystemPrompt()
	if !strings.Contains(got, recallHeaderZH) || !strings.Contains(got, "不必再 recall_memory") {
		t.Errorf("判据应说明 [想起来的事] 的用法:\n%s", got)
	}
}
