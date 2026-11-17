package imbot

import (
	"context"
	"errors"
	"sort"
)

// deliverMedia 把一件媒体投给这一轮该投的通道。判据与文字的 TurnObserver 推送
// 一致：装了分通道路由就只认路由指定的那条（投不出去也不回落——回落会把另一
// 人格窗口的内容摆到这一面），否则发给全部正在运行的通道，谁绑了这个会话就
// 发给谁。
//
// 至少一条通道成功就算成功。全都没投出去时按原因返还：真正的发送错误（上传
// 失败、通道不支持）原样返回，送达状态未知优先于它，只有所有通道都没有可用
// 出口或没人绑在这个会话上才是 ErrNoMediaChannel——后者不是硬失败，图片仍由
// 核心记进会话。把一个真实的发送失败压成「没有可送达的通道」，工具会当成
// 「已留在对话里」报成功，诊断信息也被吞掉。
func deliverMedia(ctx context.Context, sessionID string, send func(*Core, []string) error) error {
	cores, routed := mediaCores(sessionID)
	if len(cores) == 0 {
		if routed {
			return ErrMediaTargetUnavailable
		}
		return ErrNoMediaChannel
	}
	var lastErr error
	var unknown error
	sent := false
	for _, c := range cores {
		users, ok := mediaRecipients(ctx, c, sessionID, routed)
		if !ok {
			continue
		}
		err := send(c, users)
		switch {
		case err == nil:
			sent = true
		case errors.Is(err, ErrMediaDeliveryUnknown):
			unknown = err
		case errors.Is(err, ErrMediaTargetUnavailable), errors.Is(err, ErrNoMediaChannel):
			// 这条通道没有可用出口（没起来、也没人可发），不算一次投递失败
		default:
			lastErr = err
		}
	}
	if sent {
		return nil
	}
	if unknown != nil {
		return unknown
	}
	if lastErr != nil {
		return lastErr
	}
	return ErrNoMediaChannel
}

// mediaRecipients 选出某条通道上该会话的收件人。装了路由（明确指定了出口）时
// 才允许「唯一已知用户自动接入会话」：那条路正是分通道把两个人格拉进同一个会话
// 的手段。没有路由时只投给已经绑定到这个会话的人——否则用户恰好只有一个时，
// 一次后台发图就会把他从原来的会话改绑过来，连带把那边的文字也送到这里。
func mediaRecipients(ctx context.Context, c *Core, sessionID string, routed bool) ([]string, bool) {
	if routed {
		return c.recipientsFor(ctx, sessionID)
	}
	users := c.binding.UsersFor(sessionID)
	return users, len(users) > 0
}

// mediaCores 返回该会话的媒体出口，顺序稳定（按通道名）。routed 表示路由指定了
// 一条通道——那时出口只有它，缺了就是真的投不出去。
func mediaCores(sessionID string) (cores []*Core, routed bool) {
	if target := Target(sessionID); target != "" {
		regMu.RLock()
		c := liveCores[target]
		regMu.RUnlock()
		if c == nil {
			return nil, true
		}
		return []*Core{c}, true
	}
	regMu.RLock()
	names := make([]string, 0, len(liveCores))
	for name := range liveCores {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cores = append(cores, liveCores[name])
	}
	regMu.RUnlock()
	return cores, false
}
