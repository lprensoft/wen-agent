package imbot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
)

type sentFile struct {
	userID  string
	replyTo string
	file    llm.FilePart
}

type fakeFileSender struct {
	*fakeSender

	mu    sync.Mutex
	files []sentFile
	ch    chan sentFile
}

func newFakeFileSender() *fakeFileSender {
	return &fakeFileSender{fakeSender: newFakeSender(), ch: make(chan sentFile, 8)}
}

func (f *fakeFileSender) SendFile(_ context.Context, userID, replyTo string, file llm.FilePart) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := sentFile{userID: userID, replyTo: replyTo, file: file}
	f.files = append(f.files, rec)
	select {
	case f.ch <- rec:
	default:
	}
	return nil
}

func (f *fakeFileSender) wait(t *testing.T) sentFile {
	t.Helper()
	select {
	case rec := <-f.ch:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("等待发送文件超时")
		return sentFile{}
	}
}

func (f *fakeFileSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.files)
}

func TestSendTurnFileUsesTurnMessage(t *testing.T) {
	Declare("test_bot", "Test")
	sender := newFakeFileSender()
	f := llm.FilePart{Name: "a.txt", MediaType: "text/plain", Size: 5, Data: []byte("hello")}
	c := newImageCore(t, sender, func(ctx context.Context, _, _ string) (string, error) {
		ctx = plugin.WithTurnOrigin(ctx, "test_bot")
		return "已发送", SendTurnFile(ctx, f)
	})

	c.Handle(context.Background(), Message{UserID: "u1", Text: "发个文件", ReplyTo: "msg-9"})
	if m := sender.expect(t); m.text != "已发送" {
		t.Fatalf("reply = %+v", m)
	}
	got := sender.wait(t)
	if got.userID != "u1" || got.replyTo != "msg-9" {
		t.Fatalf("收件人或回复凭据不对: %+v", got)
	}
	if string(got.file.Data) != "hello" || got.file.Name != "a.txt" {
		t.Fatalf("文件内容不对: %+v", got.file)
	}
}

func TestSendTurnFileRequiresChannelSupport(t *testing.T) {
	Declare("test_bot", "Test")
	sender := newFakeSender()
	var gotErr error
	c := newImageCore(t, sender, func(ctx context.Context, _, _ string) (string, error) {
		ctx = plugin.WithTurnOrigin(ctx, "test_bot")
		gotErr = SendTurnFile(ctx, llm.FilePart{Name: "a.txt"})
		return "ok", nil
	})

	c.Handle(context.Background(), Message{UserID: "u1", Text: "发个文件"})
	sender.expect(t)
	if !errors.Is(gotErr, ErrMediaUnsupported) {
		t.Fatalf("未实现文件发送的通道应报不支持，得到 %v", gotErr)
	}
}

func TestSendTurnFileFollowsRouter(t *testing.T) {
	Declare("origin_bot", "Origin")
	Declare("target_bot", "Target")
	SetRouter(func(string) string { return "target_bot" })
	t.Cleanup(func() { SetRouter(nil) })

	originSender := newFakeFileSender()
	targetSender := newFakeFileSender()
	target := newNamedImageCore(t, "target_bot", targetSender, func(context.Context, string, string) (string, error) {
		return "ok", nil
	})
	if err := target.Bind("u2", "sess-1"); err != nil {
		t.Fatal(err)
	}

	f := llm.FilePart{Name: "a.txt", MediaType: "text/plain", Data: []byte("hello")}
	origin := newNamedImageCore(t, "origin_bot", originSender, func(ctx context.Context, _, _ string) (string, error) {
		ctx = plugin.WithTurnOrigin(ctx, "origin_bot")
		return "ok", SendTurnFile(ctx, f)
	})
	origin.Handle(context.Background(), Message{UserID: "u1", Text: "发个文件", ReplyTo: "m1"})

	got := targetSender.wait(t)
	if got.userID != "u2" || got.replyTo != "" || got.file.Name != "a.txt" {
		t.Fatalf("路由后的投递结果不符: %+v", got)
	}
	if n := originSender.count(); n != 0 {
		t.Fatalf("文件不应回到来源通道，实际 %d 个", n)
	}
}

func TestSendTurnFileDoesNotFallBackWhenTargetMissing(t *testing.T) {
	Declare("origin_bot", "Origin")
	SetRouter(func(string) string { return "ghost_bot" })
	t.Cleanup(func() { SetRouter(nil) })

	sender := newFakeFileSender()
	var gotErr error
	done := make(chan struct{})
	origin := newNamedImageCore(t, "origin_bot", sender, func(ctx context.Context, _, _ string) (string, error) {
		defer close(done)
		ctx = plugin.WithTurnOrigin(ctx, "origin_bot")
		gotErr = SendTurnFile(ctx, llm.FilePart{Name: "a.txt"})
		return "ok", nil
	})
	origin.Handle(context.Background(), Message{UserID: "u1", Text: "发个文件", ReplyTo: "m1"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("等待轮次结束超时")
	}

	if !errors.Is(gotErr, ErrMediaTargetUnavailable) {
		t.Fatalf("目标通道不存在应报投递不可用，得到 %v", gotErr)
	}
	if n := sender.count(); n != 0 {
		t.Fatalf("不能回落到来源通道，实际发了 %d 个", n)
	}
}

type ctxRecordingFileSender struct {
	*fakeSender
	got chan error
}

func (s *ctxRecordingFileSender) SendFile(ctx context.Context, _, _ string, _ llm.FilePart) error {
	s.got <- ctx.Err()
	return ctx.Err()
}

func TestPushFileKeepsTurnCancellation(t *testing.T) {
	Declare("ctx_bot", "Context")
	sender := &ctxRecordingFileSender{fakeSender: newFakeSender(), got: make(chan error, 1)}
	c := newNamedImageCore(t, "ctx_bot", sender, func(context.Context, string, string) (string, error) {
		return "ok", nil
	})
	if err := c.Bind("u1", "sess-1"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.pushFileTo(ctx, "sess-1", llm.FilePart{Name: "a.txt"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应传给发送器，得到 %v", err)
	}
	select {
	case got := <-sender.got:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("发送器看到的 ctx 应已取消，得到 %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有收到文件发送调用")
	}
}

func TestHandlePassesFilesToRunTurnMedia(t *testing.T) {
	Declare("test_bot", "Test")
	filesCh := make(chan []llm.FilePart, 1)
	var seq atomic.Int64
	c, err := New(Config{
		PluginName: "test_bot",
		Sender:     newFakeSender(),
		StateDir:   t.TempDir(),
		Allow:      func(string) bool { return true },
		RunTurn:    func(context.Context, string, string) (string, error) { return "ok", nil },
		RunTurnMedia: func(_ context.Context, _, _ string, _ []llm.ImagePart, files []llm.FilePart) (string, error) {
			filesCh <- files
			return "ok", nil
		},
		NewSession: func() (string, error) { return fmt.Sprintf("sess-%d", seq.Add(1)), nil },
		Sessions:   fakeSessions{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Start(context.Background())
	defer c.Stop()

	f := llm.FilePart{Name: "a.txt", MediaType: "text/plain", Size: 5, Data: []byte("hello")}
	c.Handle(context.Background(), Message{UserID: "u1", Files: []llm.FilePart{f}})

	select {
	case got := <-filesCh:
		if len(got) != 1 || got[0].Name != "a.txt" || string(got[0].Data) != "hello" {
			t.Fatalf("传给模型的文件不对: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("文件没有交给 RunTurnMedia")
	}
}
