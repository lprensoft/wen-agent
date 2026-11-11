package imbot

import (
	"context"

	"wen/internal/llm"
	"wen/internal/plugin"
)

// SendTurnFile 把一个普通文件发回当前对话的出口。出口按分通道路由裁决，
// 路由指定了别的通道时发到那条通道；投不出去就返回失败，绝不回落到来源通道。
func SendTurnFile(ctx context.Context, f llm.FilePart) error {
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
		return core.sendTurnFile(ctx, f)
	}
	return core.pushFileTo(ctx, sessionID, f)
}

func (c *Core) sendTurnFile(ctx context.Context, f llm.FilePart) error {
	msg, ok := turnMessageFrom(ctx)
	if !ok {
		return ErrNoMediaChannel
	}
	sender, ok := c.cfg.Sender.(FileSender)
	if !ok {
		return ErrMediaUnsupported
	}
	if !c.Running() {
		return ErrNoMediaChannel
	}
	return sender.SendFile(ctx, msg.UserID, msg.ReplyTo, f)
}

// pushFileTo 把文件发到路由指定的另一条通道，收件人选择与图片一致。
func (c *Core) pushFileTo(ctx context.Context, sessionID string, f llm.FilePart) error {
	sender, ok := c.cfg.Sender.(FileSender)
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
		if err := sender.SendFile(pctx, u, "", f); err != nil {
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
