package imbot

import (
	"context"
	"errors"

	"wen/internal/llm"
	"wen/internal/plugin"
)

var (
	// ErrNoMediaChannel 表示本轮不是从一条运行中的 IM 通道发起的（Web UI、
	// 后台轮次或已停用的通道），没有可用的收件人与回复凭据。
	ErrNoMediaChannel = errors.New("当前对话不是从 IM 通道发起的")
	// ErrMediaUnsupported 表示这条通道实现了文本发送，但还没实现该媒体的发送。
	ErrMediaUnsupported = errors.New("该通道不支持发送该媒体")
	// ErrMediaTargetUnavailable 表示分通道路由指定的出口当前不可用或没有收件人。
	// 媒体不会回落到来源通道，而是明确失败。
	ErrMediaTargetUnavailable = errors.New("媒体投递通道不可用")
	// ErrMediaDeliveryUnknown 表示媒体可能已经送达，但平台没有确认。调用方不得
	// 自动重发，模型也应被告知不要重试，否则对方会收到两份。
	ErrMediaDeliveryUnknown = errors.New("媒体送达状态未知")
)

// turnMessageKey 把本轮入站消息带进轮次 ctx，供轮次中的工具发图时取收件人与
// 回复凭据。文本回复不需要它：那条路径本来就是拿着 msg 直接发的。
type turnMessageKey struct{}

// turnSessionKey 记录本轮的会话 ID，发图时用它与分通道路由裁决出口。
type turnSessionKey struct{}

func withTurnMessage(ctx context.Context, msg Message) context.Context {
	return context.WithValue(ctx, turnMessageKey{}, msg)
}

func withTurnSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, turnSessionKey{}, sessionID)
}

func turnMessageFrom(ctx context.Context) (Message, bool) {
	msg, ok := ctx.Value(turnMessageKey{}).(Message)
	return msg, ok
}

func turnSessionFrom(ctx context.Context) string {
	sid, _ := ctx.Value(turnSessionKey{}).(string)
	return sid
}

// SendTurnImage 把一张图片发回当前对话的出口。出口按分通道路由裁决：
// 路由指定了别的通道时，图片发到那条通道；投不出去就返回失败，绝不回落到
// 来源通道——否则另一人格窗口里会出现不属于它的内容。路由未指定时按
// 「回复来源消息」处理，复用本轮的回复凭据。
func SendTurnImage(ctx context.Context, img llm.ImagePart) error {
	origin := plugin.TurnOriginFrom(ctx)
	if origin == "" || !IsChannel(origin) {
		return ErrNoMediaChannel
	}
	if _, ok := turnMessageFrom(ctx); !ok {
		return ErrNoMediaChannel
	}
	sessionID := turnSessionFrom(ctx)
	if sessionID == "" {
		return ErrNoMediaChannel
	}

	target := Target(sessionID)
	if target == "" {
		target = origin
	}
	regMu.RLock()
	core := liveCores[target]
	regMu.RUnlock()
	if core == nil {
		return ErrMediaTargetUnavailable
	}
	if target == origin {
		return core.sendTurnImage(ctx, img)
	}
	return core.pushImageTo(ctx, sessionID, img)
}

func (c *Core) sendTurnImage(ctx context.Context, img llm.ImagePart) error {
	msg, ok := turnMessageFrom(ctx)
	if !ok {
		return ErrNoMediaChannel
	}
	sender, ok := c.cfg.Sender.(ImageSender)
	if !ok {
		return ErrMediaUnsupported
	}
	if !c.Running() {
		return ErrNoMediaChannel
	}
	return sender.SendImage(ctx, msg.UserID, msg.ReplyTo, img)
}

// pushImageTo 把图片发到路由指定的另一条通道。收件人的选法与文本转投一致
// （见 recipientsFor）；没有收件人或通道未运行就报错，不退回来源通道。
func (c *Core) pushImageTo(ctx context.Context, sessionID string, img llm.ImagePart) error {
	sender, ok := c.cfg.Sender.(ImageSender)
	if !ok {
		return ErrMediaUnsupported
	}
	if !c.Running() {
		return ErrMediaTargetUnavailable
	}
	users, ok := c.recipientsFor(ctx, sessionID)
	if !ok {
		return ErrMediaTargetUnavailable
	}
	pctx, cancel := c.mediaCtx(ctx)
	defer cancel()
	var lastErr error
	sent := false
	for _, u := range users {
		if err := sender.SendImage(pctx, u, "", img); err != nil {
			lastErr = err
			continue
		}
		sent = true
	}
	if sent {
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return ErrMediaTargetUnavailable
}

// mediaCtx 保留本轮的工具 ctx（含截止时间与取消信号），同时在本通道停止时取消。
// 媒体工具是同步调用，不能像文本转投那样脱离轮次继续跑：用户取消或轮次超时后，
// 上传与发送都该停下来。
func (c *Core) mediaCtx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	life := c.ctx
	c.mu.Unlock()
	if life == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(life, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}
