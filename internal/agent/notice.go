package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// 会话注记：插件在一个会话上做完了什么，用一行话留在那个会话里给人看。
//
// 存在的理由是后台工作与轮次不同步。插件发起的活儿（提炼、清扫这类）在轮次收尾
// 之后才跑完，那时这一轮的事件流早已关闭，结果无处可去，只能进日志——而日志不是
// 用户会去翻的地方。注记补上这条通路：落盘进会话、经 NoticeSink 实时送到界面，
// 但永不进入模型上下文（见 session.KindNotice）。
//
// 核心不解释注记的内容，也不知道「记忆」「提炼」这回事，只负责存与送。

// noticeMaxRunes 给注记一个上界。它每条都要落盘并推给界面，而写它的是插件，
// 核心不该指望对方自觉。
const noticeMaxRunes = 500

// NoticeSink 接收刚落盘的会话注记，供界面实时展示。
// 在插件的后台 goroutine 上被调用，实现必须快速返回、不得阻塞。
type NoticeSink func(sessionID string, m session.StoredMessage)

// SetNoticeSink 安装注记的实时出口；传 nil 表示不需要（注记仍照常落盘）。
func (a *Agent) SetNoticeSink(fn NoticeSink) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.noticeSink = fn
}

// AppendNotice 往会话里追加一条只给人看的注记。
//
// 标签取自 ctx 里的可见域：一条「在人格 A 的库里记了什么」的注记不该被人格 B 看见。
// 发起方同样取自 ctx，由 Manager 注入，插件无法伪装成前台。
//
// 刻意不取该会话的轮次锁。注记来自后台 goroutine，而工具的 Execute 也可能想写一条
// ——那时轮次锁正被本轮持有，取锁就是自锁。代价是与并发的压缩 Replace 有竞争，
// 极小概率丢掉一条注记；注记是旁注不是对话内容，丢一条不影响任何后续行为。
func (a *Agent) AppendNotice(ctx context.Context, sessionID, text string) error {
	text = strings.TrimSpace(text)
	if sessionID == "" {
		return fmt.Errorf("%s", i18n.T(a.opts.Lang, "agent.err.need_session_id", "需要提供会话 id"))
	}
	if text == "" {
		return nil // 没话说不是错误，省掉调用方一处判断
	}
	m := session.StoredMessage{
		Message: llm.Message{Role: llm.RoleSystem, Content: truncateRunes(a.opts.Lang, text, noticeMaxRunes)},
		Kind:    session.KindNotice,
		Tag:     plugin.ScopeFrom(ctx).Write,
		Origin:  plugin.TurnOriginFrom(ctx),
		TS:      time.Now(),
	}
	if err := a.store.Append(sessionID, m); err != nil {
		return err
	}

	a.mu.RLock()
	sink := a.noticeSink
	a.mu.RUnlock()
	if sink != nil {
		sink(sessionID, m)
	}

	// 落盘与界面送达之后再广播给插件（NoticeObserver）：IM 通道据此把后台工作的
	// 说明推给远端用户。广播的是已截断的落盘文本，推出去的与界面看到的一字不差。
	a.plugins.NotifyNotice(ctx, plugin.NoticeEvent{
		SessionID: sessionID,
		Origin:    m.Origin,
		Tag:       m.Tag,
		Text:      m.Message.Content,
	})
	return nil
}
