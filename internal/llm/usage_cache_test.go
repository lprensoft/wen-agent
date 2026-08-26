package llm

import (
	"context"
	"testing"
)

// DeepSeek 的缓存是服务端自动开的，命中数放在顶层 prompt_cache_hit_tokens。
func TestUsageCacheHitDeepSeekShape(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"好"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":45,"total_tokens":10045,"prompt_cache_hit_tokens":9600,"prompt_cache_miss_tokens":400}}`,
	}, &map[string]any{})
	defer srv.Close()

	u := collectUsage(t, srv.URL)
	if u.CachedTokens != 9600 {
		t.Errorf("命中数 = %d，期望 9600", u.CachedTokens)
	}
	// 这两家的缓存写入不单独计费，写入侧恒为 0
	if u.CacheWriteTokens != 0 {
		t.Errorf("写入侧应为 0，实际 %d", u.CacheWriteTokens)
	}
	if u.PromptTokens != 10000 {
		t.Errorf("PromptTokens = %d", u.PromptTokens)
	}
}

// OpenAI 把同一信息放在 prompt_tokens_details.cached_tokens。
func TestUsageCacheHitOpenAIShape(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"好"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":45,"prompt_tokens_details":{"cached_tokens":8000}}}`,
	}, &map[string]any{})
	defer srv.Close()

	if u := collectUsage(t, srv.URL); u.CachedTokens != 8000 {
		t.Errorf("命中数 = %d，期望 8000", u.CachedTokens)
	}
}

// 不报缓存的服务照旧工作，命中数为 0。
func TestUsageWithoutCacheFields(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[],"usage":{"prompt_tokens":123,"completion_tokens":45,"total_tokens":168}}`,
	}, &map[string]any{})
	defer srv.Close()

	u := collectUsage(t, srv.URL)
	if u.CachedTokens != 0 || u.PromptTokens != 123 || u.CompletionTokens != 45 {
		t.Errorf("usage = %+v", u)
	}
}

func collectUsage(t *testing.T, url string) *Usage {
	t.Helper()
	ch, err := NewOpenAICompat("", url, "sk-test", "").ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "问"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var u *Usage
	for ev := range ch {
		if ev.Type == EventUsage {
			u = ev.Usage
		}
	}
	if u == nil {
		t.Fatal("没有收到用量事件")
	}
	return u
}
