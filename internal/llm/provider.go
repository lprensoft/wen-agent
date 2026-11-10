package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
)

// Role 常量与 OpenAI 协议一致。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

type Message struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning_content,omitempty"` // 思考内容；带工具调用时必须回传给 API
	// ReasoningBlocks 保留 provider 原始的思考块（含签名），跨轮回传时需要。
	// 仅 Anthropic 使用；OpenAI 兼容模式忽略此字段，只用 Reasoning。
	ReasoningBlocks []ReasoningBlock `json:"reasoning_blocks,omitempty"`
	// ResponsesItems 是 Responses API 上一轮输出的原始 Item 数组。推理项里的
	// encrypted_content 与 function_call 的 call_id 只能原样回传，重新拼装会丢。
	// 仅 protocol=responses 使用；其它 provider 忽略。
	ResponsesItems json.RawMessage `json:"responses_items,omitempty"`
	// ResponsesOrigin 标记这些 Item 是哪个 base_url + model 产出的。切到别的
	// 提供商或模型时不能把推理项原样塞回去，buildRequest 会改走消息重建。
	ResponsesOrigin string `json:"responses_origin,omitempty"`
	// Images 是随消息一起发送的图片。Data 只在进程内使用，落盘时由会话层换成
	// Name 引用；后续轮次从会话附件目录按需读回。
	Images     []ImagePart `json:"images,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`   // assistant 发起的工具调用
	ToolCallID string      `json:"tool_call_id,omitempty"` // role=tool 时对应的调用 ID
}

// ImagePart 是一张随消息发送的图片。
type ImagePart struct {
	Name      string `json:"name,omitempty"`       // 会话附件文件名；进程内可为空
	MediaType string `json:"media_type,omitempty"` // image/png、image/jpeg、image/gif、image/webp
	Size      int    `json:"size,omitempty"`       // 原始字节数，供界面展示
	Data      []byte `json:"-"`                    // 只在进程内保留
}

func imageDataURL(img ImagePart) string {
	return "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

// FileName 给出上传时用的文件名：保留原名的可读部分，扩展名按实际 MediaType
// 校正。各 IM 通道对文件名与扩展名的要求不同，但都不该由模型随手给的名字决定
// 上传内容的类型。
func (img ImagePart) FileName() string {
	name := filepath.Base(strings.TrimSpace(img.Name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "image"
	}
	origExt := filepath.Ext(name)
	ext := strings.ToLower(origExt)
	stem := strings.TrimSuffix(name, origExt)
	if stem == "" {
		stem = "image"
	}
	switch strings.ToLower(img.MediaType) {
	case "image/png":
		if ext == ".png" {
			return stem + origExt
		}
		return stem + ".png"
	case "image/jpeg":
		if ext == ".jpg" || ext == ".jpeg" {
			return stem + origExt
		}
		return stem + ".jpg"
	case "image/gif":
		if ext == ".gif" {
			return stem + origExt
		}
		return stem + ".gif"
	default:
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif":
			return stem + origExt
		}
		return stem + ".jpg"
	}
}

// ReasoningBlock 是一个原样保留的思考块。Anthropic 要求带签名原样回传，
// 签名缺失或被改动会被 API 拒绝。
type ReasoningBlock struct {
	Type      string `json:"type"`                // thinking / redacted_thinking
	Text      string `json:"text,omitempty"`      // 思考文本
	Signature string `json:"signature,omitempty"` // 思考块签名
	Data      string `json:"data,omitempty"`      // redacted_thinking 的密文
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolSpec 描述一个可供模型调用的工具。
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage // 参数 JSON Schema
}

type ChatRequest struct {
	Model       string
	Messages    []Message
	Tools       []ToolSpec
	Temperature float64
	MaxTokens   int
	// Thinking 思考模式："off" 关闭；"low"/"medium"/"high"/"xhigh"/"max" 为思考强度
	//（DeepSeek 服务端将 medium/xhigh 归并为 high）。思考开启时 Temperature 不生效。
	Thinking string
}

type EventType int

const (
	EventContentDelta   EventType = iota // Content 为文本增量
	EventReasoningDelta                  // Content 为思考内容增量
	EventToolCalls                       // ToolCalls 为累积完整的工具调用
	EventReasoning                       // Reasoning 为累积完整的思考块（含签名）
	EventUsage                           // Usage 为本次请求的实测 token 用量
	EventDone
	EventError
)

// Usage 是 API 返回的实测 token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens 是 PromptTokens 中命中提示词缓存、按更低价计费的部分。
	// 各家字段名不同（DeepSeek 的 prompt_cache_hit_tokens、OpenAI 的
	// prompt_tokens_details.cached_tokens、Anthropic 的 cache_read_input_tokens），
	// 由各 provider 归一到这里。0 表示未命中，或该提供商不报此项。
	CachedTokens int `json:"cached_tokens,omitempty"`
	// CacheWriteTokens 是本次写入缓存的 token 数，仅 Anthropic 有此概念且单独计费
	// （约 1.25 倍输入价）；DeepSeek 与 OpenAI 的缓存由服务端自动维护、写入不计费，恒为 0。
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

type StreamEvent struct {
	Type      EventType
	Content   string
	ToolCalls []ToolCall
	Reasoning []ReasoningBlock
	Usage     *Usage
	// RawItems 是 Responses API 本次输出的完整 Item 数组，随 EventDone 下发。
	// Agent 原样落盘到助手消息的 ResponsesItems，下一轮回放。
	RawItems json.RawMessage
	// RawOrigin 是 RawItems 的产出来源（base_url + model），用于避免把推理项
	// 回放给另一个提供商或模型。
	RawOrigin string
	Err       error
}

// Provider 是 LLM 后端的统一接口。
// ChatStream 返回的 channel 依次吐出事件，流结束（含出错）后关闭。
type Provider interface {
	ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
}
