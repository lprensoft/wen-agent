package session

import (
	"time"

	"wen/internal/llm"
)

// Usage 是最近一次 LLM 请求的实测 token 用量（来自 API usage 字段）。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// CachedTokens 是 PromptTokens 中命中提示词缓存的部分，CacheWriteTokens 是本次
	// 写入缓存的部分（仅 Anthropic 有写入概念）。留着是为了能看出缓存到底有没有在
	// 起作用——上下文的稳定前缀是靠约定维持的，一处提示词挪错位置就会静默失效。
	CachedTokens     int `json:"cached_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// Meta 是每个 session 文件的第一行记录。
type Meta struct {
	Type      string    `json:"type"` // 恒为 "meta"
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	// LastActiveAt 是最近一次有真人交互的轮次时间（前台界面、远程 IM）。
	// 机器自发的轮次（定时触发等）不更新它，「最近活跃会话」据此判定。
	// 旧会话没有该字段，使用方应回落到 CreatedAt。
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
	// LastMessageAt 是最近一条消息落盘的时间，含机器自发的轮次与后台注记。
	// 与 LastActiveAt 不同：那个只记真人交互，这个用来判断界面上有没有新内容。
	// 旧会话没有该字段，界面按「没有未读」处理。
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	LastUsage     *Usage     `json:"last_usage,omitempty"`
	// Tag 是生成该标题的那一轮消息所属的可见域标签（空串 = 共享）。
	// 标题取自首条用户消息，因此它和消息一样需要归属，否则会话检索会把不可读域的
	// 内容通过标题漏出去。
	Tag string `json:"tag,omitempty"`
}

// activeAt 是「最近活跃」的排序键：最近一次真人交互的时间，旧会话没有该字段时
// 回落创建时间。List 的排序与 LastActive 的挑选共用这一条规则，不各写一份。
func (m Meta) activeAt() time.Time {
	if m.LastActiveAt != nil {
		return *m.LastActiveAt
	}
	return m.CreatedAt
}

// KindSummary 标记压缩摘要消息（Kind 为空则是普通消息）。
const KindSummary = "summary"

// KindEphemeral 标记一次性输入：机器注入的提示（如心跳），只在它自己那一轮
// 发给模型，后续轮次组装上下文时被过滤，界面也不按用户消息展示。
const KindEphemeral = "ephemeral"

// KindNotice 标记会话注记：只给人看的一行说明（如后台任务在这个会话上做了什么），
// 落盘并在界面展示，但永不进入模型上下文，也不进压缩摘要。
//
// 与 KindEphemeral 正好相反：那个是「给模型看一轮、界面不当用户消息展示」，
// 这个是「界面一直展示、模型一次也看不到」。
const KindNotice = "notice"

// StoredMessage 是 JSONL 中的一条消息记录（meta 行之后）。
type StoredMessage struct {
	llm.Message
	Kind string `json:"kind,omitempty"` // "" 普通消息；KindSummary 压缩摘要
	Tag  string `json:"tag,omitempty"`  // 可见域标签，空串 = 共享（所有域可读）
	// Origin 是本轮对话的发起方插件名，空串 = 前台界面。供审计与界面区分后台轮次。
	Origin string    `json:"origin,omitempty"`
	TS     time.Time `json:"ts"`
}
