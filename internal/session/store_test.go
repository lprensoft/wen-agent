package session

import (
	"testing"
	"time"

	"wen/internal/llm"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateAppendGet(t *testing.T) {
	s := newTestStore(t)
	meta, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID == "" || meta.Type != "meta" {
		t.Fatalf("bad meta: %+v", meta)
	}

	msgs := []StoredMessage{
		{Message: llm.Message{Role: "user", Content: "你好"}, TS: time.Now()},
		{Message: llm.Message{Role: "assistant", Content: "你好！\n有什么可以帮你？"}, TS: time.Now()},
	}
	for _, m := range msgs {
		if err := s.Append(meta.ID, m); err != nil {
			t.Fatal(err)
		}
	}

	gotMeta, gotMsgs, err := s.Get(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotMeta.ID != meta.ID {
		t.Errorf("meta id = %q, want %q", gotMeta.ID, meta.ID)
	}
	if len(gotMsgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(gotMsgs))
	}
	if gotMsgs[1].Content != "你好！\n有什么可以帮你？" {
		t.Errorf("content mismatch: %q", gotMsgs[1].Content)
	}
}

func TestTagRoundTrip(t *testing.T) {
	s := newTestStore(t)
	meta, _ := s.Create()

	if err := s.Append(meta.ID, StoredMessage{
		Message: llm.Message{Role: "user", Content: "带标签"}, Tag: "inner", TS: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(meta.ID, StoredMessage{
		Message: llm.Message{Role: "user", Content: "无标签"}, TS: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTitleTagged(meta.ID, "标题", "inner"); err != nil {
		t.Fatal(err)
	}

	gotMeta, msgs, err := s.Get(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotMeta.Title != "标题" || gotMeta.Tag != "inner" {
		t.Errorf("meta = %+v", gotMeta)
	}
	if msgs[0].Tag != "inner" || msgs[1].Tag != "" {
		t.Errorf("消息标签未正确落盘: %q %q", msgs[0].Tag, msgs[1].Tag)
	}

	// SetUsage 等其它 meta 改动不应把标签抹掉（updateMeta 是整行重写的）
	if err := s.SetUsage(meta.ID, &Usage{PromptTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if gotMeta, _, _ = s.Get(meta.ID); gotMeta.Tag != "inner" {
		t.Errorf("其它 meta 改动把标签抹掉了: %+v", gotMeta)
	}

	// Replace 保留 meta 行，也就保留标签
	if err := s.Replace(meta.ID, []StoredMessage{
		{Message: llm.Message{Role: "user", Content: "摘要"}, Kind: KindSummary, Tag: "inner", TS: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	gotMeta, msgs, _ = s.Get(meta.ID)
	if gotMeta.Tag != "inner" || len(msgs) != 1 || msgs[0].Tag != "inner" || msgs[0].Kind != KindSummary {
		t.Errorf("Replace 后: meta=%+v msgs=%+v", gotMeta, msgs)
	}
}

func TestSetTitleAndList(t *testing.T) {
	s := newTestStore(t)
	m1, _ := s.Create()
	time.Sleep(1100 * time.Millisecond) // ID 精度到秒，确保排序可区分
	m2, _ := s.Create()

	if err := s.SetTitle(m1.ID, "第一个会话"); err != nil {
		t.Fatal(err)
	}
	s.Append(m1.ID, StoredMessage{Message: llm.Message{Role: "user", Content: "hi"}, TS: time.Now()})

	metas, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("got %d sessions, want 2", len(metas))
	}
	// 新的在前
	if metas[0].ID != m2.ID {
		t.Errorf("list order wrong: %v", metas)
	}
	if metas[1].Title != "第一个会话" {
		t.Errorf("title = %q", metas[1].Title)
	}
	// SetTitle 不应破坏已有消息
	_, msgs, _ := s.Get(m1.ID)
	if len(msgs) != 1 {
		t.Errorf("messages lost after SetTitle: %d", len(msgs))
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.Create()
	if err := s.Delete(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(m.ID); err == nil {
		t.Fatal("expected error after delete")
	}
	metas, _ := s.List()
	if len(metas) != 0 {
		t.Errorf("list should be empty, got %d", len(metas))
	}
}

func TestSetUsage(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.Create()
	s.Append(m.ID, StoredMessage{Message: llm.Message{Role: "user", Content: "hi"}, TS: time.Now()})

	if err := s.SetUsage(m.ID, &Usage{PromptTokens: 100, CompletionTokens: 20}); err != nil {
		t.Fatal(err)
	}
	meta, msgs, _ := s.Get(m.ID)
	if meta.LastUsage == nil || meta.LastUsage.PromptTokens != 100 {
		t.Errorf("usage = %+v", meta.LastUsage)
	}
	if len(msgs) != 1 {
		t.Errorf("messages affected by SetUsage: %d", len(msgs))
	}

	// 清除
	if err := s.SetUsage(m.ID, nil); err != nil {
		t.Fatal(err)
	}
	meta, _, _ = s.Get(m.ID)
	if meta.LastUsage != nil {
		t.Errorf("usage not cleared: %+v", meta.LastUsage)
	}
}

func TestReplace(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.Create()
	s.SetTitle(m.ID, "标题")
	for i := 0; i < 3; i++ {
		s.Append(m.ID, StoredMessage{Message: llm.Message{Role: "user", Content: "旧消息"}, TS: time.Now()})
	}

	err := s.Replace(m.ID, []StoredMessage{
		{Message: llm.Message{Role: "user", Content: "摘要"}, Kind: "summary", TS: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}

	meta, msgs, err := s.Get(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "标题" {
		t.Errorf("meta title lost: %q", meta.Title)
	}
	if len(msgs) != 1 || msgs[0].Kind != "summary" || msgs[0].Content != "摘要" {
		t.Errorf("replaced messages = %+v", msgs)
	}
}

func TestInvalidID(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"", "../evil", `a\b`, "a/b"} {
		if _, _, err := s.Get(id); err == nil {
			t.Errorf("id %q should be rejected", id)
		}
	}
}

func TestListOrdersByActivity(t *testing.T) {
	s := newTestStore(t)
	old, _ := s.Create()
	time.Sleep(1100 * time.Millisecond) // ID 精度到秒，确保创建时间可区分
	fresh, _ := s.Create()

	// 旧会话后来有人来过：它该排到刚创建但没人交互的会话前面
	if err := s.SetLastActive(old.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	metas, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 || metas[0].ID != old.ID || metas[1].ID != fresh.ID {
		t.Fatalf("list order: got %v, want [%s %s]", ids(metas), old.ID, fresh.ID)
	}
	// LastActive 与 List 用同一条规则
	id, _, err := s.LastActive()
	if err != nil || id != old.ID {
		t.Errorf("LastActive = %q, %v; want %q", id, err, old.ID)
	}
}

func ids(metas []Meta) []string {
	out := make([]string, len(metas))
	for i, m := range metas {
		out[i] = m.ID
	}
	return out
}

func TestSessionImagesLifecycle(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("\x89PNG\r\n\x1a\nfake")
	name, err := s.PutImage(meta.ID, "image/png", data)
	if err != nil {
		t.Fatalf("PutImage: %v", err)
	}
	got, err := s.ReadImage(meta.ID, name)
	if err != nil || string(got) != string(data) {
		t.Fatalf("ReadImage = %q, %v", got, err)
	}
	if _, err := s.PutImage(meta.ID, "image/tiff", data); err == nil {
		t.Fatal("unsupported media type should be rejected")
	}
	if _, err := s.ImagePath(meta.ID, "../escape"); err == nil {
		t.Fatal("path traversal should be rejected")
	}
	if err := s.Delete(meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadImage(meta.ID, name); err == nil {
		t.Fatal("image should be removed together with its session")
	}
}
