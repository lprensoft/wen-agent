package stylewatch

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/plugin"
	"wen/internal/stylecheck"
)

// noticeSink 收集插件写出的注记。
type noticeSink struct {
	mu    sync.Mutex
	items []string
	tags  []string
}

func (n *noticeSink) fn(ctx context.Context, _ string, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.items = append(n.items, text)
	n.tags = append(n.tags, plugin.ScopeFrom(ctx).Write)
	return nil
}

func (n *noticeSink) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.items...)
}

func newTestPlugin(t *testing.T, dir string, cfg map[string]any, sink *noticeSink) *Plugin {
	t.Helper()
	p := New()
	ictx := plugin.InitContext{StateDir: dir}
	if sink != nil {
		ictx.Notice = sink.fn
	}
	if err := p.Init(ictx, cfg); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	return p
}

func turn(interactive bool, text string) plugin.TurnEndEvent {
	now := time.Now()
	return plugin.TurnEndEvent{
		SessionID: "s1", Interactive: interactive, UserInput: "嗯", FinalText: text,
		StartedAt: now, EndedAt: now,
	}
}

// feed 喂一轮并等它落账（Stop 会等在途 goroutine），随后重新 Init 让插件可继续用。
func feed(t *testing.T, p *Plugin, dir string, cfg map[string]any, sink *noticeSink, evs ...plugin.TurnEndEvent) {
	t.Helper()
	for _, ev := range evs {
		p.OnTurnEnd(context.Background(), ev)
	}
	p.Stop()
	ictx := plugin.InitContext{StateDir: dir}
	if sink != nil {
		ictx.Notice = sink.fn
	}
	if err := p.Init(ictx, cfg); err != nil {
		t.Fatalf("重新 Init 失败: %v", err)
	}
}

func TestInitRejectsWithoutStateDir(t *testing.T) {
	if err := New().Init(plugin.InitContext{}, nil); err == nil {
		t.Fatal("没有持久化目录时应当拒绝启用")
	}
}

func TestInitRejectsUnknownIgnoreRule(t *testing.T) {
	err := New().Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"ignore_rules": "emoji\nno_such\n"})
	if err == nil || !strings.Contains(err.Error(), "no_such") {
		t.Fatalf("未知规则应报错并点名，实际 %v", err)
	}
	// 空行与注释行跳过
	if err := New().Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"ignore_rules": "\n# 注释\nemoji\n"}); err != nil {
		t.Fatalf("合法的忽略表不该报错: %v", err)
	}
}

func TestSystemPromptEmpty(t *testing.T) {
	if New().SystemPrompt() != "" {
		t.Fatal("本期不对模型说任何话")
	}
}

func TestCountsInteractiveAndBackgroundSeparately(t *testing.T) {
	dir := t.TempDir()
	sink := &noticeSink{}
	p := newTestPlugin(t, dir, nil, sink)
	feed(t, p, dir, nil, sink,
		turn(true, "【放下杯子】好的，我听着。希望这对你有帮助。"),
		turn(true, "【笑】不急。"),
		turn(false, "记得**吃药** 😊"),
		turn(true, ""), // 空回复不计
	)

	today, ok := p.st.find(dateOf(time.Now()))
	if !ok {
		t.Fatal("今天应有统计")
	}
	fg, bg := today.Interactive, today.Background
	if fg.Turns != 2 || fg.HitTurns != 1 || fg.totalHits() != 2 || fg.Hits["opening_cliche"] != 1 || fg.Hits["closing_cliche"] != 1 {
		t.Errorf("真人轮次统计 = %+v", fg)
	}
	if bg.Turns != 1 || bg.totalHits() != 2 || bg.Hits["md_bold"] != 1 || bg.Hits["emoji"] != 1 {
		t.Errorf("后台轮次统计 = %+v", bg)
	}
	if fg.MinRunes != 3 || fg.MaxRunes <= fg.MinRunes {
		t.Errorf("最短/最长 = %d/%d", fg.MinRunes, fg.MaxRunes)
	}

	// 落盘了，且重新加载能读回来
	raw, err := os.ReadFile(statsPath(dir))
	if err != nil {
		t.Fatalf("统计未落盘: %v", err)
	}
	var st stats
	if err := json.Unmarshal(raw, &st); err != nil || len(st.Days) != 1 || st.Days[0].Interactive.Turns != 2 {
		t.Errorf("落盘内容 = %s", raw)
	}
	fresh := newTestPlugin(t, dir, nil, nil)
	if d, ok := fresh.st.find(dateOf(time.Now())); !ok || d.Interactive.Turns != 2 {
		t.Errorf("重新 Init 后应读回统计: %+v", d)
	}

	// 注记：只给命中的轮次写，一轮一条，合并多处
	got := sink.all()
	if len(got) != 2 {
		t.Fatalf("注记数 = %d，期望 2（真人一条、后台一条）: %v", len(got), got)
	}
	// 两条注记出自两个 goroutine，先后不定，按内容找
	joined := strings.Join(got, "\n")
	for _, want := range []string{"✍️ 文风提醒：客套开场「好的，」、客套收尾「希望这对你有帮助」", "✍️ 文风提醒：加粗「**吃药**」、emoji「😊」"} {
		if !strings.Contains(joined, want) {
			t.Errorf("注记缺 %q：%v", want, got)
		}
	}

	line := p.StatusLines("")[0]
	for _, want := range []string{"✍️ 文风：今日 2 轮", "助手腔 2 次", "客套开场 1", "客套收尾 1", "均长", "演绎占", "后台 1 轮 2 次"} {
		if !strings.Contains(line, want) {
			t.Errorf("状态行缺 %q：%s", want, line)
		}
	}
}

func TestStatusLineWithoutData(t *testing.T) {
	p := newTestPlugin(t, t.TempDir(), nil, nil)
	if got := p.StatusLines(""); len(got) != 1 || got[0] != "✍️ 文风：今日尚无数据" {
		t.Errorf("状态行 = %v", got)
	}
}

func TestIgnoreRulesAndNotifyOff(t *testing.T) {
	dir := t.TempDir()
	sink := &noticeSink{}
	cfg := map[string]any{"ignore_rules": "emoji\nmd_bold", "notify": false}
	p := newTestPlugin(t, dir, cfg, sink)
	feed(t, p, dir, cfg, sink, turn(true, "记得**吃药** 😊 希望这对你有帮助。"))

	today, _ := p.st.find(dateOf(time.Now()))
	if today.Interactive.totalHits() != 1 || today.Interactive.Hits["closing_cliche"] != 1 {
		t.Errorf("被忽略的规则不该计入：%+v", today.Interactive.Hits)
	}
	if n := len(sink.all()); n != 0 {
		t.Errorf("notify 关闭时不该写注记，实际 %d 条", n)
	}
}

func TestNoticeCarriesScopeAndIsClipped(t *testing.T) {
	dir := t.TempDir()
	sink := &noticeSink{}
	p := newTestPlugin(t, dir, nil, sink)
	ctx := plugin.WithScope(context.Background(), plugin.Scope{Write: "inner", Read: []string{"inner"}})
	p.OnTurnEnd(ctx, turn(true, strings.Repeat("😊", 300)))
	p.Stop()

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("注记数 = %d", len(got))
	}
	if r := []rune(got[0]); len(r) > noticeMaxRunes {
		t.Errorf("注记长度 %d 超过上限 %d", len(r), noticeMaxRunes)
	}
	if !strings.Contains(got[0], "emoji ×300") {
		t.Errorf("同一规则多处应合并成次数：%q", got[0])
	}
	if sink.tags[0] != "inner" {
		t.Errorf("注记应带本轮的可见域标签，实际 %q", sink.tags[0])
	}
}

func TestRenderNoticeMergesByRule(t *testing.T) {
	got := renderNotice("", []stylecheck.Hit{
		{Rule: "emoji", Label: "emoji", Snippet: "😊"},
		{Rule: "md_bold", Label: "加粗", Snippet: "**重点**"},
		{Rule: "emoji", Label: "emoji", Snippet: "😂"},
	})
	if got != "✍️ 文风提醒：emoji ×2「😊」、加粗「**重点**」" {
		t.Errorf("注记 = %q", got)
	}
}

func TestKeepsOnlyRecentDays(t *testing.T) {
	var st stats
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)
	for i := 0; i < keepDays+5; i++ {
		d := st.day(dateOf(base.AddDate(0, 0, i)))
		d.Interactive.add(stylecheck.Metrics{Runes: 10}, nil)
	}
	if len(st.Days) != keepDays {
		t.Fatalf("保留天数 = %d，期望 %d", len(st.Days), keepDays)
	}
	if st.Days[0].Date != dateOf(base.AddDate(0, 0, 5)) {
		t.Errorf("应丢掉最早的日子，首日 = %s", st.Days[0].Date)
	}
	// 比保留期还早的日子不进表，也不崩
	st.day("2000-01-01").Interactive.add(stylecheck.Metrics{}, nil)
	if len(st.Days) != keepDays || st.index("2000-01-01") >= 0 {
		t.Error("过早的日子不该留在表里")
	}
}

func TestReportAndActions(t *testing.T) {
	dir := t.TempDir()
	p := newTestPlugin(t, dir, nil, nil)
	tool := p.Tools()[0]
	if tool.Name() != "style_report" {
		t.Fatalf("工具名 = %s", tool.Name())
	}
	if out, _ := tool.Execute(context.Background(), nil); !strings.Contains(out, "没有记录") {
		t.Errorf("无数据时的报告 = %q", out)
	}

	feed(t, p, dir, nil, nil, turn(true, "好的，知道了。"), turn(false, "【看表】到点了。"))
	out, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"最近 7 天", "（今天）：1 轮，助手腔 1 次（客套开场 1）", "后台 1 轮 0 次", "合计 1 轮，助手腔 1 次（客套开场 1）"} {
		if !strings.Contains(out, want) {
			t.Errorf("报告缺 %q：\n%s", want, out)
		}
	}

	if err := p.StartAction(context.Background(), actionReport); err != nil {
		t.Fatal(err)
	}
	if st, _ := p.ActionState(actionReport); st.Status != plugin.ActionDone || !strings.Contains(st.Message, "最近 30 天") {
		t.Errorf("报告操作状态 = %+v", st)
	}
	if st, _ := p.ActionState(actionClear); st.Status != plugin.ActionIdle {
		t.Errorf("未触发的操作应为 idle：%+v", st)
	}
	if err := p.StartAction(context.Background(), actionClear); err != nil {
		t.Fatal(err)
	}
	if got := p.StatusLines("")[0]; got != "✍️ 文风：今日尚无数据" {
		t.Errorf("清空后状态行 = %q", got)
	}
	if fresh := newTestPlugin(t, dir, nil, nil); len(fresh.st.Days) != 0 {
		t.Error("清空应当落盘")
	}
	if err := p.StartAction(context.Background(), "nope"); err == nil {
		t.Error("未知操作应报错")
	}
}

// 配置项说明里列出了全部规则 id，用户不必去翻代码。
func TestConfigFieldListsRules(t *testing.T) {
	var desc string
	for _, f := range New().ConfigFields() {
		if f.Key == "ignore_rules" {
			desc = f.Description
		}
	}
	for _, r := range stylecheck.Rules() {
		if !strings.Contains(desc, r.ID+"（"+r.Label.ZH()+"）") {
			t.Errorf("配置说明缺规则 %s", r.ID)
		}
	}
}

// Stop 之后再来的轮次不再登记；重新 Init 后恢复。
func TestStopThenReinit(t *testing.T) {
	dir := t.TempDir()
	p := newTestPlugin(t, dir, nil, nil)
	p.Stop()
	p.OnTurnEnd(context.Background(), turn(true, "好的，来了。"))
	if len(p.st.Days) != 0 {
		t.Error("停止后不该再计")
	}
	if err := p.Init(plugin.InitContext{StateDir: dir}, nil); err != nil {
		t.Fatal(err)
	}
	feed(t, p, dir, nil, nil, turn(true, "好的，来了。"))
	if d, ok := p.st.find(dateOf(time.Now())); !ok || d.Interactive.Turns != 1 {
		t.Errorf("重新 Init 后应恢复统计：%+v", d)
	}
}
