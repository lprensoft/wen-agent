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

type sentImage struct {
	userID  string
	replyTo string
	img     llm.ImagePart
}

// fakeImageSender 在文本发送能力之外实现图片发送，并记下每次调用。
type fakeImageSender struct {
	*fakeSender

	mu     sync.Mutex
	images []sentImage
	ch     chan sentImage
	err    error
}

func newFakeImageSender() *fakeImageSender {
	return &fakeImageSender{fakeSender: newFakeSender(), ch: make(chan sentImage, 8)}
}

func (f *fakeImageSender) SendImage(_ context.Context, userID, replyTo string, img llm.ImagePart) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	rec := sentImage{userID: userID, replyTo: replyTo, img: img}
	f.images = append(f.images, rec)
	select {
	case f.ch <- rec:
	default:
	}
	return nil
}

func (f *fakeImageSender) wait(t *testing.T) sentImage {
	t.Helper()
	select {
	case rec := <-f.ch:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("等待发图超时")
		return sentImage{}
	}
}

func (f *fakeImageSender) last(t *testing.T) sentImage {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.images) == 0 {
		t.Fatal("没有发出的图片")
	}
	return f.images[len(f.images)-1]
}

func (f *fakeImageSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.images)
}

func newImageCore(t *testing.T, sender Sender, runTurn plugin.RunTurnFunc) *Core {
	t.Helper()
	return newNamedImageCore(t, "test_bot", sender, runTurn)
}

func newNamedImageCore(t *testing.T, name string, sender Sender, runTurn plugin.RunTurnFunc) *Core {
	t.Helper()
	var seq atomic.Int64
	c, err := New(Config{
		PluginName:     name,
		Sender:         sender,
		StateDir:       t.TempDir(),
		ConfirmTimeout: 2 * time.Second,
		TurnTimeout:    5 * time.Second,
		Allow:          func(u string) bool { return u == "u1" || u == "u2" },
		Push:           func(context.Context, string, string) bool { return true },
		RunTurn:        runTurn,
		NewSession:     func() (string, error) { return fmt.Sprintf("sess-%d", seq.Add(1)), nil },
		Sessions:       fakeSessions{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Start(context.Background())
	t.Cleanup(c.Stop)
	return c
}

func TestSendTurnImageUsesTurnMessage(t *testing.T) {
	Declare("test_bot", "Test")
	sender := newFakeImageSender()
	img := llm.ImagePart{Name: "a.png", MediaType: "image/png", Size: 3, Data: []byte("png")}
	c := newImageCore(t, sender, func(ctx context.Context, _, _ string) (string, error) {
		ctx = plugin.WithTurnOrigin(ctx, "test_bot")
		return "已发送", SendTurnImage(ctx, img)
	})

	c.Handle(context.Background(), Message{UserID: "u1", Text: "发张图", ReplyTo: "msg-9"})
	if m := sender.expect(t); m.text != "已发送" {
		t.Fatalf("reply = %+v", m)
	}
	got := sender.last(t)
	if got.userID != "u1" || got.replyTo != "msg-9" {
		t.Fatalf("收件人或回复凭据不对: %+v", got)
	}
	if got.img.MediaType != "image/png" || string(got.img.Data) != "png" {
		t.Fatalf("图片内容不对: %+v", got.img)
	}
}

func TestSendTurnImageRejectsNonChannelTurn(t *testing.T) {
	Declare("test_bot", "Test")
	sender := newFakeImageSender()
	var gotErr error
	c := newImageCore(t, sender, func(ctx context.Context, _, _ string) (string, error) {
		gotErr = SendTurnImage(ctx, llm.ImagePart{MediaType: "image/png"})
		return "ok", nil
	})

	c.Handle(context.Background(), Message{UserID: "u1", Text: "发张图"})
	sender.expect(t)
	if !errors.Is(gotErr, ErrNoImageChannel) {
		t.Fatalf("Web UI 轮次应拒绝发图，得到 %v", gotErr)
	}
}

func TestSendTurnImageRejectsBackgroundTurn(t *testing.T) {
	Declare("test_bot", "Test")
	sender := newFakeImageSender()
	var gotErr error
	c := newImageCore(t, sender, func(ctx context.Context, _, _ string) (string, error) {
		ctx = plugin.WithTurnOrigin(ctx, "heartbeat")
		gotErr = SendTurnImage(ctx, llm.ImagePart{MediaType: "image/png"})
		return "ok", nil
	})

	c.Handle(context.Background(), Message{UserID: "u1", Text: "发张图"})
	sender.expect(t)
	if !errors.Is(gotErr, ErrNoImageChannel) {
		t.Fatalf("后台轮次应拒绝发图，得到 %v", gotErr)
	}
}

func TestSendTurnImageRequiresChannelSupport(t *testing.T) {
	Declare("test_bot", "Test")
	sender := newFakeSender()
	var gotErr error
	c := newImageCore(t, sender, func(ctx context.Context, _, _ string) (string, error) {
		ctx = plugin.WithTurnOrigin(ctx, "test_bot")
		gotErr = SendTurnImage(ctx, llm.ImagePart{MediaType: "image/png"})
		return "ok", nil
	})

	c.Handle(context.Background(), Message{UserID: "u1", Text: "发张图"})
	sender.expect(t)
	if !errors.Is(gotErr, ErrImageUnsupported) {
		t.Fatalf("未实现图片发送的通道应报不支持，得到 %v", gotErr)
	}
}

func TestSendTurnImageFollowsRouter(t *testing.T) {
	Declare("origin_bot", "Origin")
	Declare("target_bot", "Target")
	SetRouter(func(string) string { return "target_bot" })
	t.Cleanup(func() { SetRouter(nil) })

	originSender := newFakeImageSender()
	targetSender := newFakeImageSender()
	target := newNamedImageCore(t, "target_bot", targetSender, func(context.Context, string, string) (string, error) {
		return "ok", nil
	})
	if err := target.Bind("u2", "sess-1"); err != nil {
		t.Fatal(err)
	}

	img := llm.ImagePart{Name: "a.png", MediaType: "image/png", Data: []byte("png")}
	origin := newNamedImageCore(t, "origin_bot", originSender, func(ctx context.Context, _, _ string) (string, error) {
		ctx = plugin.WithTurnOrigin(ctx, "origin_bot")
		return "ok", SendTurnImage(ctx, img)
	})
	origin.Handle(context.Background(), Message{UserID: "u1", Text: "发张图", ReplyTo: "m1"})

	got := targetSender.wait(t)
	if got.userID != "u2" || got.replyTo != "" || got.img.MediaType != "image/png" {
		t.Fatalf("路由后的投递结果不符: %+v", got)
	}
	if n := originSender.count(); n != 0 {
		t.Fatalf("图片不应回到来源通道，实际 %d 张", n)
	}
}

func TestSendTurnImageDoesNotFallBackWhenTargetMissing(t *testing.T) {
	Declare("origin_bot", "Origin")
	SetRouter(func(string) string { return "ghost_bot" })
	t.Cleanup(func() { SetRouter(nil) })

	sender := newFakeImageSender()
	var gotErr error
	done := make(chan struct{})
	origin := newNamedImageCore(t, "origin_bot", sender, func(ctx context.Context, _, _ string) (string, error) {
		defer close(done)
		ctx = plugin.WithTurnOrigin(ctx, "origin_bot")
		gotErr = SendTurnImage(ctx, llm.ImagePart{MediaType: "image/png"})
		return "ok", nil
	})
	origin.Handle(context.Background(), Message{UserID: "u1", Text: "发张图", ReplyTo: "m1"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("等待轮次结束超时")
	}

	if !errors.Is(gotErr, ErrImageTargetUnavailable) {
		t.Fatalf("目标通道不存在应报投递不可用，得到 %v", gotErr)
	}
	if n := sender.count(); n != 0 {
		t.Fatalf("不能回落到来源通道，实际发了 %d 张", n)
	}
}

// ctxRecordingSender 记录图片发送时看到的 ctx 状态。
type ctxRecordingSender struct {
	*fakeSender
	got chan error
}

func (s *ctxRecordingSender) SendImage(ctx context.Context, _, _ string, _ llm.ImagePart) error {
	s.got <- ctx.Err()
	return ctx.Err()
}

func TestPushImageKeepsTurnCancellation(t *testing.T) {
	Declare("ctx_bot", "Context")
	sender := &ctxRecordingSender{fakeSender: newFakeSender(), got: make(chan error, 1)}
	c := newNamedImageCore(t, "ctx_bot", sender, func(context.Context, string, string) (string, error) {
		return "ok", nil
	})
	if err := c.Bind("u1", "sess-1"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.pushImageTo(ctx, "sess-1", llm.ImagePart{MediaType: "image/png"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应传给发送器，得到 %v", err)
	}
	select {
	case got := <-sender.got:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("发送器看到的 ctx 应已取消，得到 %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有收到图片发送调用")
	}
}
