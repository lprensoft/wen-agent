package plugin

import (
	"context"
	"errors"
	"time"

	"wen/internal/llm"
)

// ErrSessionBusy 表示会话正被另一轮对话或压缩占用。
// 插件经 InitContext.RunTurn / Compact 发起的轮次不排队：忙时立即返回本错误，
// 由插件自行决定跳过还是稍后重试（用 errors.Is 判断）。
var ErrSessionBusy = errors.New("会话正被其他轮次占用")

// RunTurnFunc 以给定输入在指定会话上跑一轮完整对话（写入会话、带工具、注入 system 提示词），
// 返回助手的最终文本。见 InitContext.RunTurn。
type RunTurnFunc func(ctx context.Context, sessionID, input string) (string, error)

// RunTurnImagesFunc 与 RunTurnFunc 相同，但本轮用户消息额外携带图片。
// 见 InitContext.RunTurnImages。
type RunTurnImagesFunc func(ctx context.Context, sessionID, input string, images []llm.ImagePart) (string, error)

// NewSessionFunc 新建一个会话并返回其 ID。见 InitContext.NewSession。
type NewSessionFunc func() (string, error)

// CompactFunc 压缩指定会话的历史。见 InitContext.Compact。
type CompactFunc func(ctx context.Context, sessionID string) error

// NoticeFunc 往会话里追加一行只给人看的注记。见 InitContext.Notice。
type NoticeFunc func(ctx context.Context, sessionID, text string) error

// turnOriginKey / interactiveKey 是轮次属性的 ctx 键。
type turnOriginKey struct{}
type interactiveKey struct{}

// WithTurnOrigin 标记本轮对话的发起方（插件名）。前台界面发起的轮次不带该标记。
// Manager 在把 RunTurn 交给插件前会自动注入插件名，插件不必也无法自行伪装。
func WithTurnOrigin(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, turnOriginKey{}, source)
}

// TurnOriginFrom 返回本轮对话的发起方；空串表示前台界面。
func TurnOriginFrom(ctx context.Context) string {
	if v, ok := ctx.Value(turnOriginKey{}).(string); ok {
		return v
	}
	return ""
}

// WithInteractive 标记本轮对话有真人在交互（前台界面、远程 IM 等）。
// 机器自发的轮次（如定时触发）不带该标记，据此区分「用户活跃」与「后台活动」。
func WithInteractive(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveKey{}, true)
}

// IsInteractive 返回本轮对话是否有真人在交互。
func IsInteractive(ctx context.Context) bool {
	v, _ := ctx.Value(interactiveKey{}).(bool)
	return v
}

type ephemeralKey struct{}

// WithEphemeralInput 标记本轮输入是机器注入的一次性提示（如心跳提示词）：
// 只在本轮发给模型，落盘后不进入后续轮次的上下文，界面也不按用户消息展示。
// 助手的回复不受影响，照常保留。
func WithEphemeralInput(ctx context.Context) context.Context {
	return context.WithValue(ctx, ephemeralKey{}, true)
}

// IsEphemeralInput 返回本轮输入是否为一次性提示。
func IsEphemeralInput(ctx context.Context) bool {
	v, _ := ctx.Value(ephemeralKey{}).(bool)
	return v
}

// 轮次过程通知的种类。
const (
	NoteThinking  = "thinking"   // 一段完整的思考链
	NoteToolCalls = "tool_calls" // 一批将要执行的工具调用
)

// TurnNote 是轮次进行中的过程性通知：每次模型响应产出的完整思考链、每批工具调用。
// 与流式 emit 不同，通知以「完整块」为粒度——面向的是无法流式渲染的接收方（远程 IM）。
type TurnNote struct {
	Kind  string
	Text  string   // Kind=NoteThinking 时为思考全文
	Tools []string // Kind=NoteToolCalls 时为本批工具名（不含参数与结果，那些可能载有隐私）
}

// TurnNoteFunc 接收过程通知。在轮次的同步路径上被调用，实现应快速返回或自行异步。
type TurnNoteFunc func(TurnNote)

type turnNotesKey struct{}

// WithTurnNotes 为本轮对话安装过程通知回调。发起方（如 IM 插件）借此把思考过程
// 与工具调用转发到远端；不安装则核心不产生任何通知开销。
func WithTurnNotes(ctx context.Context, fn TurnNoteFunc) context.Context {
	return context.WithValue(ctx, turnNotesKey{}, fn)
}

// TurnNotesFrom 取出本轮的过程通知回调，未安装返回 nil。
func TurnNotesFrom(ctx context.Context) TurnNoteFunc {
	if fn, ok := ctx.Value(turnNotesKey{}).(TurnNoteFunc); ok {
		return fn
	}
	return nil
}

// TurnEndEvent 描述一轮成功结束的对话。
type TurnEndEvent struct {
	SessionID   string
	Origin      string // 发起方插件名；空串 = 前台界面
	Interactive bool   // 本轮是否有真人在交互
	UserInput   string
	FinalText   string // 助手的最终文本
	StartedAt   time.Time
	EndedAt     time.Time
}

// TurnObserver 是插件的可选能力：观察每轮对话的结束。
// 实现必须快速返回——广播发生在轮次收尾的同步路径上，耗时工作应自行开 goroutine。
type TurnObserver interface {
	OnTurnEnd(ctx context.Context, ev TurnEndEvent)
}

// Stoppable 是插件的可选能力：停止自己启动的后台活动（定时器、长连接等）。
// 契约：只做取消与有界等待（不超过 5 秒量级），不得等待整轮对话完成。
// Manager 在禁用插件、以新配置重新 Init 之前、进程退出时调用。
type Stoppable interface {
	Stop()
}
