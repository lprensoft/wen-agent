package imbot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

// reactSender 记录收到的表情回应，并可指定下一次的结果。
type reactSender struct {
	calls []string
	err   error
}

func (s *reactSender) Send(context.Context, string, string, string) {}

func (s *reactSender) React(_ context.Context, userID, replyTo, reaction string) error {
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, userID+"|"+replyTo+"|"+reaction)
	return nil
}

// plainSender 只会发文本，没有表情回应接口。
type plainSender struct{}

func (plainSender) Send(context.Context, string, string, string) {}

func TestReactionOncePerTurn(t *testing.T) {
	s := &reactSender{}
	c := &Core{cfg: Config{Sender: s}}
	fn := c.turnReactFunc(context.Background(), Message{UserID: "u1", ReplyTo: "m1"}, "", &reactState{})

	if err := fn(ReactLike); err != nil {
		t.Fatalf("第一次回应应当成功: %v", err)
	}
	if err := fn(ReactLaugh); !errors.Is(err, ErrReactionAlreadySent) {
		t.Fatalf("同一轮第二次回应应当被拒绝，得到 %v", err)
	}
	if len(s.calls) != 1 || s.calls[0] != "u1|m1|赞" {
		t.Fatalf("发送记录异常: %v", s.calls)
	}
}

func TestReactionFailureDoesNotCountAsSent(t *testing.T) {
	s := &reactSender{err: errors.New("平台拒绝")}
	c := &Core{cfg: Config{Sender: s}}
	fn := c.turnReactFunc(context.Background(), Message{UserID: "u1", ReplyTo: "m1"}, "", &reactState{})

	if err := fn(ReactLike); err == nil {
		t.Fatal("平台失败应当返回错误")
	}
	s.err = nil
	if err := fn(ReactLike); err != nil {
		t.Fatalf("失败之后应当允许再试一次: %v", err)
	}
}

func TestReactionUnsupportedChannelAndNoTarget(t *testing.T) {
	plain := &Core{cfg: Config{Sender: plainSender{}}}
	if err := plain.turnReactFunc(context.Background(), Message{UserID: "u1", ReplyTo: "m1"}, "", &reactState{})(ReactLike); !errors.Is(err, ErrReactionUnsupported) {
		t.Fatalf("没有表情接口的通道应当报不支持，得到 %v", err)
	}

	s := &reactSender{}
	c := &Core{cfg: Config{Sender: s}}
	if err := c.turnReactFunc(context.Background(), Message{UserID: "u1"}, "", &reactState{})(ReactLike); !errors.Is(err, ErrNoReactionTarget) {
		t.Fatalf("没有入站消息凭据时应当报没有目标，得到 %v", err)
	}
}

// reactingFake 在 fakeSender 之上补上表情回应，让 Core 把它当作 ReactionSender。
type reactingFake struct{ *fakeSender }

func (reactingFake) React(context.Context, string, string, string) error { return nil }

// 回归：表情回应成功之后，本轮即使没有文字，也不能再补「本轮没有文本回复」。
func TestReactedTurnWithoutTextStaysSilent(t *testing.T) {
	c, f := newCore(t, func(ctx context.Context, _, _ string) (string, error) {
		fn := plugin.ReplyReactionFrom(ctx)
		if fn == nil {
			return "", fmt.Errorf("没有表情回应回调")
		}
		if err := fn(ReactLaugh); err != nil {
			return "", err
		}
		return "", nil
	}, func(cfg *Config) { cfg.Sender = reactingFake{cfg.Sender.(*fakeSender)} })
	c.Handle(context.Background(), Message{UserID: "u1", Text: "哈哈", ReplyTo: "m1"})
	f.expectNone(t, 500*time.Millisecond)
}

// 对照：没有回应过的空回复仍然要有提示，不能因为改动被一并吞掉。
func TestEmptyTurnWithoutReactionStillNotesNoReply(t *testing.T) {
	c, f := newCore(t, func(context.Context, string, string) (string, error) {
		return "", nil
	}, func(cfg *Config) { cfg.Sender = reactingFake{cfg.Sender.(*fakeSender)} })
	c.Handle(context.Background(), Message{UserID: "u1", Text: "哈哈", ReplyTo: "m1"})
	if m := f.expect(t); !strings.Contains(m.text, "没有文本回复") {
		t.Fatalf("未回应的空回复应当提示: %q", m.text)
	}
}
