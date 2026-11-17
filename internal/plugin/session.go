package plugin

import "context"

// 本轮所在的会话。插件在轮次里需要它做「这件事发生在哪里」的判断，而多数路径
// （尤其是后台轮次）没有入站消息可依附：心跳、定时任务只知道自己被要求在哪个
// 会话上说话，拿不到那条会话的 ID 就问不到「谁在等这条消息」。由 Agent 在每轮
// 入口注入，前台与后台一视同仁。

type sessionIDKey struct{}

// WithSessionID 标记本轮对话所在的会话。由 Agent 在轮次入口统一注入。
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFrom 返回本轮所在的会话；没有（不在轮次内）时返回空串。
func SessionIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(sessionIDKey{}).(string)
	return v
}
