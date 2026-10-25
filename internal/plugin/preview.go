package plugin

import "context"

// 回复预览：本轮模型正在写的那段回复，边写边交给发起方。
//
// 与过程通知（WithTurnNotes）同一种形状——发起方按需安装、不装零开销——区别在粒度：
// 过程通知按完整块送（一段思考链、一批工具名），这里按增量送，供能「边写边显示」的
// 远端界面用（Telegram 的草稿预览）。核心只负责把文字交出去，显示不显示、多久刷新
// 一次、往哪儿显示都归接收方。
//
// 每次送的是**当前这次模型响应到目前为止的全文**，不是增量：一轮里调了工具会有好几次
// 模型响应，后一次从空开始，接收方据此就知道前面那段作废了（多半是「我查一下」之类的
// 过渡话，最终回复里没有它），不需要另一个「重置」信号。

// ReplyPreviewFunc 接收回复预览。在模型流式输出的同步路径上被调用，必须立即返回。
type ReplyPreviewFunc func(text string)

type replyPreviewKey struct{}

// WithReplyPreview 为本轮对话安装回复预览回调。
func WithReplyPreview(ctx context.Context, fn ReplyPreviewFunc) context.Context {
	return context.WithValue(ctx, replyPreviewKey{}, fn)
}

// ReplyPreviewFrom 取出本轮的回复预览回调，未安装返回 nil。
func ReplyPreviewFrom(ctx context.Context) ReplyPreviewFunc {
	if fn, ok := ctx.Value(replyPreviewKey{}).(ReplyPreviewFunc); ok {
		return fn
	}
	return nil
}
