package imbot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/llm"
)

// turnRecorder 记下每轮的输入，供合并窗口的测试核对「几条合成了一轮」。
type turnRecorder struct {
	mu     sync.Mutex
	inputs []string
	ch     chan string
}

func newTurnRecorder() *turnRecorder { return &turnRecorder{ch: make(chan string, 64)} }

func (r *turnRecorder) run(_ context.Context, _, input string) (string, error) {
	r.mu.Lock()
	r.inputs = append(r.inputs, input)
	r.mu.Unlock()
	r.ch <- input
	return "收到：" + input, nil
}

func (r *turnRecorder) expect(t *testing.T) string {
	t.Helper()
	select {
	case in := <-r.ch:
		return in
	case <-time.After(3 * time.Second):
		t.Fatal("等待轮次超时")
		return ""
	}
}

func (r *turnRecorder) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case in := <-r.ch:
		t.Fatalf("不该再有轮次，却收到 %q", in)
	case <-time.After(d):
	}
}

const testWindow = 120 * time.Millisecond

func newMergingCore(t *testing.T, rec *turnRecorder) (*Core, *fakeSender) {
	t.Helper()
	return newCore(t, rec.run, func(cfg *Config) { cfg.MergeWindow = testWindow })
}

// 窗口内连发的三条合成一轮：正文按到达顺序换行相连，回复凭据取最后一条的。
func TestMergeWindowJoinsBurst(t *testing.T) {
	rec := newTurnRecorder()
	c, f := newMergingCore(t, rec)
	ctx := context.Background()

	c.Handle(ctx, Message{UserID: "u1", DedupID: "a", ReplyTo: "r1", Text: "在吗"})
	c.Handle(ctx, Message{UserID: "u1", DedupID: "b", ReplyTo: "r2", Text: "想问个事"})
	c.Handle(ctx, Message{UserID: "u1", DedupID: "c", ReplyTo: "r3", Text: "明天有空吗"})

	if in := rec.expect(t); in != "在吗\n想问个事\n明天有空吗" {
		t.Fatalf("三条应合成一轮: %q", in)
	}
	if m := f.expect(t); m.replyTo != "r3" {
		t.Fatalf("回复凭据应取最后一条的: %+v", m)
	}
	rec.expectNone(t, 2*testWindow)
}

// 每来一条重新计时，但总等待封顶 3 倍窗口：对方一直在打字也不能无限等。
func TestMergeWindowCaps(t *testing.T) {
	rec := newTurnRecorder()
	c, _ := newMergingCore(t, rec)
	ctx := context.Background()

	const n = 12
	step := testWindow / 2
	for i := 0; i < n; i++ {
		c.Handle(ctx, Message{UserID: "u1", DedupID: fmt.Sprint(i), Text: fmt.Sprintf("第%d条", i)})
		time.Sleep(step)
	}
	// 连发持续 6 倍窗口，封顶 3 倍，所以必然不止一轮，且第一轮里没有全部消息
	first := rec.expect(t)
	if lines := strings.Count(first, "\n") + 1; lines >= n || lines < 2 {
		t.Fatalf("第一轮应只含触顶前的那些（多于 1 条、少于 %d 条），实际 %d 条: %q", n, lines, first)
	}
	got := strings.Count(first, "第")
	deadline := time.After(3 * time.Second)
	for got < n {
		select {
		case in := <-rec.ch:
			got += strings.Count(in, "第")
		case <-deadline:
			t.Fatalf("其余消息没有成轮，累计只收到 %d 条", got)
		}
	}
	rec.expectNone(t, 2*testWindow)
}

// 命令不参与合并、立即处理；窗口里已攒的先作为一轮入队，再处理命令，顺序与到达一致。
func TestMergeWindowCommandFlushesPending(t *testing.T) {
	rec := newTurnRecorder()
	c, f := newMergingCore(t, rec)
	ctx := context.Background()

	c.Handle(ctx, Message{UserID: "u1", DedupID: "a", Text: "先说一句"})
	c.Handle(ctx, Message{UserID: "u1", DedupID: "b", Text: "/help"})

	if in := rec.expect(t); in != "先说一句" {
		t.Fatalf("命令到达时已攒的应立即成轮: %q", in)
	}
	if m := f.expect(t); m.text != "收到：先说一句" {
		t.Fatalf("先回对话: %+v", m)
	}
	if m := f.expect(t); !strings.Contains(m.text, "/compact") {
		t.Fatalf("再回命令: %+v", m)
	}
}

// 关闭窗口时逐条成轮，行为与没有这个功能时一致。
func TestMergeWindowDisabledIsPerMessage(t *testing.T) {
	rec := newTurnRecorder()
	c, _ := newCore(t, rec.run, nil)
	ctx := context.Background()

	c.Handle(ctx, Message{UserID: "u1", DedupID: "a", Text: "一"})
	c.Handle(ctx, Message{UserID: "u1", DedupID: "b", Text: "二"})
	got := map[string]bool{rec.expect(t): true, rec.expect(t): true}
	if !got["一"] || !got["二"] {
		t.Fatalf("关闭窗口时应各成一轮: %v", got)
	}
}

// 轮次跑着的时候攒下的几条，出队时同样合成一轮。
func TestMergeWindowDrainsQueueOnDequeue(t *testing.T) {
	rec := newTurnRecorder()
	release := make(chan struct{})
	var first sync.Once
	c, _ := newCore(t, func(ctx context.Context, sid, input string) (string, error) {
		first.Do(func() { <-release }) // 第一轮卡住，后面的在队里排队
		return rec.run(ctx, sid, input)
	}, func(cfg *Config) { cfg.MergeWindow = testWindow })
	ctx := context.Background()

	c.Handle(ctx, Message{UserID: "u1", DedupID: "a", Text: "第一轮"})
	time.Sleep(2 * testWindow) // 等它出窗进入 worker 并卡住
	c.Handle(ctx, Message{UserID: "u1", DedupID: "b", Text: "补一句"})
	time.Sleep(2 * testWindow) // 出窗、入队
	c.Handle(ctx, Message{UserID: "u1", DedupID: "c", Text: "再补一句"})
	time.Sleep(2 * testWindow)
	close(release)

	if in := rec.expect(t); in != "第一轮" {
		t.Fatalf("第一轮应单独: %q", in)
	}
	if in := rec.expect(t); in != "补一句\n再补一句" {
		t.Fatalf("排队的两条出队时应合成一轮: %q", in)
	}
}

// OnAccepted 对每条入站消息照常调用：微信靠它记住每条的 context_token。
func TestMergeWindowCallsOnAcceptedPerMessage(t *testing.T) {
	rec := newTurnRecorder()
	var got []string
	var mu sync.Mutex
	c, _ := newCore(t, rec.run, func(cfg *Config) {
		cfg.MergeWindow = testWindow
		cfg.OnAccepted = func(m Message) {
			mu.Lock()
			got = append(got, m.ReplyTo)
			mu.Unlock()
		}
	})
	ctx := context.Background()
	c.Handle(ctx, Message{UserID: "u1", DedupID: "a", ReplyTo: "r1", Text: "一"})
	c.Handle(ctx, Message{UserID: "u1", DedupID: "b", ReplyTo: "r2", Text: "二"})
	rec.expect(t)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "r1" || got[1] != "r2" {
		t.Fatalf("每条都该记账: %v", got)
	}
}

func TestMergeMessages(t *testing.T) {
	one := Message{UserID: "u", ReplyTo: "r", Text: "只一条"}
	if got := mergeMessages([]Message{one}); got.UserID != one.UserID || got.ReplyTo != one.ReplyTo || got.Text != one.Text {
		t.Fatalf("单条原样返回: %+v", got)
	}
	got := mergeMessages([]Message{
		{UserID: "u", ReplyTo: "r1", Text: " 甲 "},
		{UserID: "u", ReplyTo: "r2", Text: ""},
		{UserID: "u", ReplyTo: "r3", Text: "乙"},
	})
	if got.Text != "甲\n乙" || got.ReplyTo != "r3" {
		t.Fatalf("合并结果异常: %+v", got)
	}
}

func TestMergeMessagesKeepsImages(t *testing.T) {
	got := mergeMessages([]Message{
		{UserID: "u", Text: "一", Images: []llm.ImagePart{{Name: "a", MediaType: "image/png"}}},
		{UserID: "u", Text: "二", Images: []llm.ImagePart{{Name: "b", MediaType: "image/png"}}},
	})
	if len(got.Images) != 2 || got.Images[0].Name != "a" || got.Images[1].Name != "b" {
		t.Fatalf("images = %+v", got.Images)
	}
}

func TestMergeMessagesKeepsNotes(t *testing.T) {
	got := mergeMessages([]Message{
		{UserID: "u", Text: "看图", Note: "图片下载失败"},
		{UserID: "u", Text: "还有这个", Note: "文件超过大小上限"},
	})
	if got.Note != "图片下载失败\n文件超过大小上限" {
		t.Fatalf("note = %q", got.Note)
	}
}

func TestMergeMessagesKeepsFiles(t *testing.T) {
	got := mergeMessages([]Message{
		{UserID: "u", Text: "一", Files: []llm.FilePart{{Name: "a.txt"}}},
		{UserID: "u", Text: "二", Files: []llm.FilePart{{Name: "b.txt"}}},
	})
	if len(got.Files) != 2 || got.Files[0].Name != "a.txt" || got.Files[1].Name != "b.txt" {
		t.Fatalf("files = %+v", got.Files)
	}
}

func TestSplitByMediaBudgetSplitsFiles(t *testing.T) {
	file := func(name string) llm.FilePart {
		return llm.FilePart{Name: name, MediaType: "text/plain", Data: []byte("x")}
	}
	var msgs []Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs, Message{UserID: "u", Text: fmt.Sprintf("第%d个", i), Files: []llm.FilePart{file(fmt.Sprint(i))}})
	}
	groups := splitByMediaBudget(msgs)
	if len(groups) != 2 || len(groups[0]) != 4 || len(groups[1]) != 1 {
		t.Fatalf("groups = %d", len(groups))
	}
}

func TestSplitByImageBudgetKeepsTextWithImages(t *testing.T) {
	img := func(name string) llm.ImagePart {
		return llm.ImagePart{Name: name, MediaType: "image/png", Data: []byte("x")}
	}
	var msgs []Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs, Message{UserID: "u", Text: fmt.Sprintf("第%d张", i), Images: []llm.ImagePart{img(fmt.Sprint(i))}})
	}
	groups := splitByMediaBudget(msgs)
	if len(groups) != 2 || len(groups[0]) != 4 || len(groups[1]) != 1 {
		t.Fatalf("groups = %d/%d", len(groups), func() int { return len(groups[0]) }())
	}
	total := 0
	for _, g := range groups {
		m := mergeMessages(g)
		total += len(m.Images)
		if len(m.Images) != len(g) {
			t.Fatalf("image count = %d, want %d", len(m.Images), len(g))
		}
	}
	if total != 5 {
		t.Fatalf("lost images: %d", total)
	}
}
