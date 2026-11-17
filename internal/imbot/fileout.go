package imbot

import (
	"context"

	"wen/internal/llm"
	"wen/internal/plugin"
)

// SendTurnFile 把一个普通文件发给这一轮该发给的人。出口的判据与图片相同：
// 前台 IM 轮次按回复凭据与路由，后台轮次按会话推给绑定用户，前台界面轮次
// 没有外部出口。
func SendTurnFile(ctx context.Context, f llm.FilePart) error {
	origin := plugin.TurnOriginFrom(ctx)
	if origin != "" && IsChannel(origin) {
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
	if origin == "" {
		return ErrNoMediaChannel
	}
	sessionID := plugin.SessionIDFrom(ctx)
	if sessionID == "" {
		return ErrNoMediaChannel
	}
	return deliverMedia(ctx, sessionID, func(c *Core, users []string) error {
		return c.pushFileUsers(ctx, users, f)
	})
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
	users, ok := c.recipientsFor(ctx, sessionID)
	if !ok {
		return ErrMediaTargetUnavailable
	}
	return c.pushFileUsers(ctx, users, f)
}

// pushFileUsers 把文件发给一组已经选好的收件人，至少一人成功就算成功。
func (c *Core) pushFileUsers(ctx context.Context, users []string, f llm.FilePart) error {
	sender, ok := c.cfg.Sender.(FileSender)
	if !ok {
		return ErrMediaUnsupported
	}
	if !c.Running() {
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
