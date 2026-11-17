package plugin

import (
	"context"
)

// 本轮入站消息上的表情回应。与出站媒体同形：核心在轮次入口装回调，插件在工具执行
// 中调用。回调只接收中文规范的表情名（见 imbot 的词表），由通道自己映射成平台的表情。

// ReactFunc 给本轮对方发来的那条消息加一个表情回应。
type ReactFunc func(reaction string) error

type replyReactionKey struct{}

// WithReplyReaction 为本轮安装表情回应回调；不安装时 ReplyReactionFrom 返回 nil，
// 插件应当把它当成「这里没有可回应的消息」，而不是失败。
func WithReplyReaction(ctx context.Context, fn ReactFunc) context.Context {
	return context.WithValue(ctx, replyReactionKey{}, fn)
}

// ReplyReactionFrom 取出本轮的表情回应回调，未安装返回 nil。
func ReplyReactionFrom(ctx context.Context) ReactFunc {
	if fn, ok := ctx.Value(replyReactionKey{}).(ReactFunc); ok {
		return fn
	}
	return nil
}
