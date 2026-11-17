package imbot

import (
	"context"
	"errors"
	"sync"

	"wen/internal/plugin"
)

// 表情回应的词表。模型只能从中选一个，骨架与各通道都用这里的中文名做键；每个通道
// 自己把它映射成平台的表情（emoji、shortcode 或 emoji_type）。词表有意很小：它是
// 「一个人会随手点的几个反应」，不是完整的表情面板。
const (
	ReactLike      = "赞"
	ReactHeart     = "爱心"
	ReactLaugh     = "笑"
	ReactCry       = "哭"
	ReactSurprise  = "惊讶"
	ReactThinking  = "思考"
	ReactCelebrate = "庆祝"
	ReactCheer     = "加油"
	ReactGotIt     = "收到"
)

// ReactionVocabulary 按展示顺序列出全部表情名。
var ReactionVocabulary = []string{
	ReactLike, ReactHeart, ReactLaugh, ReactCry, ReactSurprise,
	ReactThinking, ReactCelebrate, ReactCheer, ReactGotIt,
}

var (
	// ErrReactionUnsupported 表示这条通道没有表情回应接口（如微信、QQ 单聊）。
	ErrReactionUnsupported = errors.New("该通道不支持表情回应")
	// ErrNoReactionTarget 表示本轮没有可回应的入站消息：后台轮次没有消息，
	// 或回复已经转投到别的通道。
	ErrNoReactionTarget = errors.New("当前对话没有可回应的消息")
	// ErrReactionAlreadySent 表示本轮已经回应过一次。
	ErrReactionAlreadySent = errors.New("本轮已经回应过了")
)

// ReactionSender 是 Sender 的可选能力：给入站消息加一个表情回应。replyTo 是入站消息
// 的回复凭据，reaction 是词表中的中文名，由通道映射成平台的表情。
type ReactionSender interface {
	React(ctx context.Context, userID, replyTo, reaction string) error
}

// turnReactFunc 为本轮构造表情回应回调。通道没有 ReactionSender 时回调照常安装，
// 但直接报「不支持」——对模型来说这是通道的事实，不是「这一轮没有消息」。
//
// 一轮只回应一次：多次回应只会在对方眼前堆出一排表情。失败不计入，模型可以换一个再试。
func (c *Core) turnReactFunc(ctx context.Context, msg Message, sid string, st *reactState) plugin.ReactFunc {
	rs, ok := c.cfg.Sender.(ReactionSender)
	if !ok {
		return func(string) error { return ErrReactionUnsupported }
	}
	return func(reaction string) error {
		if msg.ReplyTo == "" {
			return ErrNoReactionTarget
		}
		// 回复转投到了别的通道时，这条消息不属于那一面：表情也不能露在这一面的窗口里
		if target := Target(sid); target != "" && target != c.cfg.PluginName {
			return ErrNoReactionTarget
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.sent {
			return ErrReactionAlreadySent
		}
		rctx, cancel := c.mediaCtx(ctx)
		defer cancel()
		if err := rs.React(rctx, msg.UserID, msg.ReplyTo, reaction); err != nil {
			return err
		}
		st.sent = true
		return nil
	}
}

// reactState 记录本轮是否已经用表情回应过对方。轮次结束时据此判断：已经回应过的
// 那一轮没有文本是正常的，不应再补一句「本轮没有文本回复」。
type reactState struct {
	mu   sync.Mutex
	sent bool
}

func (s *reactState) reacted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}
