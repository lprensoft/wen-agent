package imbot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/plugin"
)

func TestSegmentSplitsParagraphsAndSentences(t *testing.T) {
	text := "今天天气不错。要不要出去走走？\n\n我知道一家新开的咖啡店……就在街角。"
	got := Segment(text)
	want := []string{"今天天气不错。要不要出去走走？", "我知道一家新开的咖啡店……就在街角。"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("段落应各成一条，段内短句合并:\n%q", got)
	}
}

func TestSegmentSplitsLongParagraphAtSentenceEnds(t *testing.T) {
	sentence := strings.Repeat("嗯", 50) + "。"
	text := sentence + sentence + sentence // 153 字，一条装不下
	got := segment(text, 120, 6)
	if len(got) != 2 || got[0] != sentence+sentence || got[1] != sentence {
		t.Fatalf("应在句末切开并尽量装满: %d 条 %q", len(got), got)
	}
}

func TestSegmentKeepsPunctuationClosers(t *testing.T) {
	got := splitSentences("他说「好。」然后走了？！真的……")
	want := []string{"他说「好。」", "然后走了？！", "真的……"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("标点与收尾引号应留在句尾: %q", got)
	}
	// 西文句点：后面要跟空白，前面不能是数字
	got = splitSentences("Version 3.5 is out. Try it! 1. first")
	want = []string{"Version 3.5 is out.", " Try it!", " 1. first"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("西文句点的判定异常: %q", got)
	}
}

func TestSegmentKeepsCodeBlockAndTableWhole(t *testing.T) {
	code := "```go\nfmt.Println(\"a。b。\")\n\nfmt.Println(\"c！\")\n```"
	table := "| a | b |\n|---|---|\n| 1。 | 2！ |"
	text := "看这段。\n\n" + code + "\n\n" + table + "\n\n完了。"
	got := Segment(text)
	if len(got) != 4 || got[1] != code || got[2] != table {
		t.Fatalf("代码块与表格应整段保留: %d 条 %q", len(got), got)
	}
	// 超长代码块也不切
	long := "```\n" + strings.Repeat("x", 500) + "\n```"
	if got := segment(long, 120, 6); len(got) != 1 || got[0] != long {
		t.Fatalf("超长代码块不该被切: %q", got)
	}
}

func TestSegmentCapsCount(t *testing.T) {
	var paras []string
	for i := 0; i < 10; i++ {
		paras = append(paras, "第"+string(rune('０'+i))+"段。")
	}
	got := segment(strings.Join(paras, "\n\n"), 120, 6)
	if len(got) != 6 {
		t.Fatalf("最多 6 条: %d", len(got))
	}
	if got[5] != strings.Join(paras[5:], "\n\n") {
		t.Fatalf("超出的应并进最后一条并保留段落间隔: %q", got[5])
	}
	// 句子级的尾巴合并时直接相连，不平白多出空行
	sentences := strings.Repeat(strings.Repeat("句", 100)+"。", 8)
	got = segment(sentences, 120, 3)
	if len(got) != 3 || strings.Contains(got[2], "\n") {
		t.Fatalf("句子尾巴应直接相连: %d 条，末条 %q", len(got), got[2])
	}
}

func TestSegmentEmptyAndNoPrefix(t *testing.T) {
	if got := Segment("   "); len(got) != 1 || got[0] != "" {
		t.Fatalf("空文本返回一条空: %q", got)
	}
	for _, s := range Segment("一。\n\n二。\n\n三。") {
		if strings.HasPrefix(s, "(") {
			t.Fatalf("分条不加 (1/N) 前缀: %q", s)
		}
	}
}

func TestPaceDelay(t *testing.T) {
	cases := map[int]time.Duration{
		0:   minTypingDelay,
		3:   minTypingDelay,
		10:  600 * time.Millisecond,
		30:  1800 * time.Millisecond,
		100: maxTypingDelay,
	}
	for runes, want := range cases {
		if got := paceDelay(runes); got != want {
			t.Errorf("%d 字: %v，期望 %v", runes, got, want)
		}
	}
}

// paceRecorder 记下分条发送过程中的等待与输入状态切换。
type paceRecorder struct {
	mu     sync.Mutex
	sleeps []time.Duration
	typing []bool
}

func (r *paceRecorder) sleep(_ context.Context, d time.Duration) {
	r.mu.Lock()
	r.sleeps = append(r.sleeps, d)
	r.mu.Unlock()
}

func (r *paceRecorder) onTyping(_ context.Context, _ Message, on bool) {
	r.mu.Lock()
	r.typing = append(r.typing, on)
	r.mu.Unlock()
}

// 开关开着：回复分条按顺序发出、每条之间按上一条字数等待并亮输入状态；
// 命令回执原样一条。
func TestHumanPaceSplitsReply(t *testing.T) {
	rec := &paceRecorder{}
	reply := "第一句好长好长好长好长好长。\n\n第二句。\n\n第三句。"
	c, f := newCore(t, func(context.Context, string, string) (string, error) {
		return reply, nil
	}, func(cfg *Config) {
		cfg.HumanPace = true
		cfg.sleep = rec.sleep
		cfg.Typing = rec.onTyping
	})
	ctx := context.Background()

	c.Handle(ctx, Message{UserID: "u1", DedupID: "a", ReplyTo: "r", Text: "说点什么"})
	var texts []string
	for range 3 {
		m := f.expect(t)
		if m.replyTo != "r" {
			t.Fatalf("每条都回带凭据: %+v", m)
		}
		texts = append(texts, m.text)
	}
	if strings.Join(texts, "|") != "第一句好长好长好长好长好长。|第二句。|第三句。" {
		t.Fatalf("分条顺序或内容异常: %q", texts)
	}
	f.expectNone(t, 200*time.Millisecond)

	rec.mu.Lock()
	sleeps, typing := append([]time.Duration(nil), rec.sleeps...), append([]bool(nil), rec.typing...)
	rec.mu.Unlock()
	// 两个间隔：按上一条的字数，14 字 → 840ms；4 字 → 下限 400ms
	if len(sleeps) != 2 || sleeps[0] != 840*time.Millisecond || sleeps[1] != minTypingDelay {
		t.Fatalf("间隔异常: %v", sleeps)
	}
	// 轮次前后一开一关，加两个间隔各一开一关
	want := []bool{true, false, true, false, true, false}
	if len(typing) != len(want) {
		t.Fatalf("输入状态切换次数异常: %v", typing)
	}
	for i := range want {
		if typing[i] != want[i] {
			t.Fatalf("输入状态序列异常: %v", typing)
		}
	}

	// 命令回执不分条、不延迟
	c.Handle(ctx, Message{UserID: "u1", DedupID: "b", Text: "/help"})
	if m := f.expect(t); m.text != HelpText("") {
		t.Fatalf("命令回执应原样一条: %q", m.text)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sleeps) != 2 {
		t.Fatalf("命令回执不该有等待: %v", rec.sleeps)
	}
}

// 开关关着：行为与从前逐字节一致——整段一条、不等待、不碰输入状态（轮次前后那一开一关除外）。
func TestHumanPaceOffSendsWhole(t *testing.T) {
	rec := &paceRecorder{}
	reply := "第一句。\n\n第二句。"
	c, f := newCore(t, func(context.Context, string, string) (string, error) {
		return reply, nil
	}, func(cfg *Config) {
		cfg.sleep = rec.sleep
		cfg.Typing = rec.onTyping
	})
	c.Handle(context.Background(), Message{UserID: "u1", Text: "说"})
	if m := f.expect(t); m.text != reply {
		t.Fatalf("应整段一条: %q", m.text)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sleeps) != 0 || len(rec.typing) != 2 {
		t.Fatalf("关着时不该有等待或额外的输入状态: %v %v", rec.sleeps, rec.typing)
	}
}

// 过程通知（思考链、工具名）不分条、不延迟，只有最终回复分条。
func TestHumanPaceLeavesNotesWhole(t *testing.T) {
	rec := &paceRecorder{}
	thinking := "先想第一步。\n\n再想第二步。"
	c, f := newCore(t, func(ctx context.Context, _, _ string) (string, error) {
		plugin.TurnNotesFrom(ctx)(plugin.TurnNote{Kind: plugin.NoteThinking, Text: thinking})
		return "好。\n\n走吧。", nil
	}, func(cfg *Config) {
		cfg.HumanPace, cfg.ShowThinking = true, true
		cfg.sleep = rec.sleep
	})
	c.Handle(context.Background(), Message{UserID: "u1", Text: "说"})
	if m := f.expect(t); m.text != ThinkingLine("", thinking) {
		t.Fatalf("思考链应原样一条: %q", m.text)
	}
	if m := f.expect(t); m.text != "好。" {
		t.Fatalf("最终回复第一条: %q", m.text)
	}
	if m := f.expect(t); m.text != "走吧。" {
		t.Fatalf("最终回复第二条: %q", m.text)
	}
}

// 后台轮次的推送也按开关分条：它同样是角色在说话。
func TestPushReplyPaces(t *testing.T) {
	rec := &paceRecorder{}
	var pushed []string
	var mu sync.Mutex
	c, _ := newCore(t, noopTurn, func(cfg *Config) {
		cfg.HumanPace = true
		cfg.sleep = rec.sleep
		cfg.Push = func(_ context.Context, userID, text string) bool {
			mu.Lock()
			pushed = append(pushed, userID+"|"+text)
			mu.Unlock()
			return true
		}
	})
	if !c.PushReply(context.Background(), "u1", "第一句。\n\n第二句。") {
		t.Fatal("应报告已交给平台")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pushed) != 2 || pushed[0] != "u1|第一句。" || pushed[1] != "u1|第二句。" {
		t.Fatalf("后台推送应分条: %v", pushed)
	}
	if len(rec.sleeps) != 1 {
		t.Fatalf("两条之间应等一次: %v", rec.sleeps)
	}
}

// ctx 取消时停发剩余分条。
func TestHumanPaceStopsOnCancel(t *testing.T) {
	var pushed []string
	ctx, cancel := context.WithCancel(context.Background())
	c, _ := newCore(t, noopTurn, func(cfg *Config) {
		cfg.HumanPace = true
		cfg.sleep = func(context.Context, time.Duration) { cancel() }
		cfg.Push = func(_ context.Context, _, text string) bool {
			pushed = append(pushed, text)
			return true
		}
	})
	c.PushReply(ctx, "u1", "一。\n\n二。\n\n三。")
	if len(pushed) != 1 {
		t.Fatalf("取消后不该继续发: %v", pushed)
	}
}

// 说话引导只在「开关开着 + 本通道发起的轮次」里注入。
func TestTurnPromptOnlyForOwnTurns(t *testing.T) {
	on, _ := newCore(t, noopTurn, func(cfg *Config) { cfg.HumanPace = true })
	off, _ := newCore(t, noopTurn, nil)

	mine := plugin.WithTurnOrigin(context.Background(), "test_bot")
	other := plugin.WithTurnOrigin(context.Background(), "other_bot")
	front := context.Background()

	if got := on.TurnPrompt(mine); got != ChatStylePrompt("") {
		t.Fatalf("本通道的轮次应注入引导: %q", got)
	}
	if got := on.TurnPrompt(other); got != "" {
		t.Fatalf("别的通道发起的轮次不注入: %q", got)
	}
	if got := on.TurnPrompt(front); got != "" {
		t.Fatalf("前台轮次不注入: %q", got)
	}
	if got := off.TurnPrompt(mine); got != "" {
		t.Fatalf("开关关着不注入: %q", got)
	}
}
