package imbot

import (
	"context"
	"sync"
	"testing"
	"time"

	"wen/internal/plugin"
)

// 节流：一阵增量只送最新的那份；stop 之后不再送，且等最后一次送完才返回
func TestPreviewerThrottlesAndStops(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	feed, stop := startPreview(context.Background(), func(_ context.Context, text string) {
		mu.Lock()
		sent = append(sent, text)
		mu.Unlock()
	})
	for _, s := range []string{"a", "ab", "abc"} {
		feed(s)
	}
	time.Sleep(previewInterval / 2)
	mu.Lock()
	first := append([]string(nil), sent...)
	mu.Unlock()
	if len(first) != 1 || first[0] != "abc" {
		t.Fatalf("应只送最新的一份: %q", first)
	}
	feed("abcd")
	stop() // 节流间隔还没到：这一份不该在 stop 之后才冒出来
	time.Sleep(previewInterval + 200*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 {
		t.Errorf("stop 之后不该再送: %q", sent)
	}
}

// 本轮在 RunTurn 里换了人格、回复要转投到别的通道时，预览不能留在这一侧
func TestPreviewFollowsRouteDecidedInsideTurn(t *testing.T) {
	resetRegistry(t)
	var mu sync.Mutex
	var previews []string
	turn := func(ctx context.Context, sid, _ string) (string, error) {
		SetRouter(func(string) string { return "other_bot" }) // 相当于 DecideScope 在轮次里切了人格
		plugin.ReplyPreviewFrom(ctx)("里面那一面的话")
		time.Sleep(previewInterval + 300*time.Millisecond)
		return "ok", nil
	}
	c, _ := newCore(t, turn, func(cfg *Config) {
		cfg.Preview = func(_ context.Context, _ Message, text string) {
			mu.Lock()
			previews = append(previews, text)
			mu.Unlock()
		}
	})
	c.Handle(context.Background(), Message{UserID: "u1", DedupID: "m1", Text: "暗号"})
	time.Sleep(3 * previewInterval)
	mu.Lock()
	defer mu.Unlock()
	if len(previews) != 0 {
		t.Fatalf("回复转投别处时这一侧不该有预览: %q", previews)
	}
}

// 不转投时照常预览
func TestPreviewShownWhenReplyStaysHere(t *testing.T) {
	resetRegistry(t)
	got := make(chan string, 4)
	turn := func(ctx context.Context, _, _ string) (string, error) {
		plugin.ReplyPreviewFrom(ctx)("写到一半")
		time.Sleep(previewInterval + 300*time.Millisecond)
		return "ok", nil
	}
	c, _ := newCore(t, turn, func(cfg *Config) {
		cfg.Preview = func(_ context.Context, _ Message, text string) { got <- text }
	})
	c.Handle(context.Background(), Message{UserID: "u1", DedupID: "m2", Text: "你好"})
	select {
	case s := <-got:
		if s != "写到一半" {
			t.Fatalf("预览内容不对: %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有预览")
	}
}
