package imbot

import (
	"context"
	"sync"
	"testing"
	"time"
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
