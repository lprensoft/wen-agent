package imbot

import (
	"context"
	"errors"
	"testing"
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
	fn := c.turnReactFunc(context.Background(), Message{UserID: "u1", ReplyTo: "m1"}, "")

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
	fn := c.turnReactFunc(context.Background(), Message{UserID: "u1", ReplyTo: "m1"}, "")

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
	if err := plain.turnReactFunc(context.Background(), Message{UserID: "u1", ReplyTo: "m1"}, "")(ReactLike); !errors.Is(err, ErrReactionUnsupported) {
		t.Fatalf("没有表情接口的通道应当报不支持，得到 %v", err)
	}

	s := &reactSender{}
	c := &Core{cfg: Config{Sender: s}}
	if err := c.turnReactFunc(context.Background(), Message{UserID: "u1"}, "")(ReactLike); !errors.Is(err, ErrNoReactionTarget) {
		t.Fatalf("没有入站消息凭据时应当报没有目标，得到 %v", err)
	}
}
