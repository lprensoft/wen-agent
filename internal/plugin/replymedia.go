package plugin

import (
	"context"

	"wen/internal/llm"
)

// 本轮回复里附带的媒体。工具把图片发给对方之后调用它，由核心落盘、记进会话，
// 并在前台界面轮次里推给界面——「她发出去的东西」因此和文字一样留在对话里，
// 而不只是出现在某一条消息通道的聊天记录中。与 Confirmer 同形：核心装回调，
// 插件在工具执行中调用。

// ReplyMediaFunc 把一张要随本轮回复发出的图片交给核心。img.Data 必须有值：
// 先送达、再登记，核心负责落盘，调用方不需要自己存。
type ReplyMediaFunc func(img llm.ImagePart) error

type replyMediaKey struct{}

// WithReplyMedia 为本轮安装出站媒体回调；不安装时 ReplyMediaFrom 返回 nil，
// 插件应把它当成「没有界面可记」降级，而不是失败。
func WithReplyMedia(ctx context.Context, fn ReplyMediaFunc) context.Context {
	return context.WithValue(ctx, replyMediaKey{}, fn)
}

// ReplyMediaFrom 取出本轮的出站媒体回调，未安装返回 nil。
func ReplyMediaFrom(ctx context.Context) ReplyMediaFunc {
	if fn, ok := ctx.Value(replyMediaKey{}).(ReplyMediaFunc); ok {
		return fn
	}
	return nil
}
