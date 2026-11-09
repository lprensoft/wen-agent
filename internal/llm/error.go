package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
)

// KindContentFilter 表示后端的内容安全拦截（如 finish_reason=content_filter）。
const KindContentFilter = "content_filter"

// KindConfig 表示状态码本身看不出、但实为配置问题的错误（如 Anthropic 密钥没绑定
// 工作区时的 400）。归进 IsConfigError，原样示人。
const KindConfig = "config"

// APIError 是后端 API 返回的错误。定型成结构而不是裸字符串，
// 是为了让上层能按状态码分流（配置类错误必须原样示人，其余才可做善后处理），
// 文本形式与定型前保持一致，界面与日志的呈现不受影响。
type APIError struct {
	Status int    // HTTP 状态码；流内错误帧没有状态码，为 0
	Kind   string // 机器可读的分类，可空；目前只有 KindContentFilter
	// Code 是流内错误帧自带的类型/代码，如 server_error、overloaded_error。
	// 流内错误没有 HTTP 状态码，只能靠它判断是否值得换备用模型。
	Code string
	Body string // 错误体原文（调用方已截断）
}

func (e *APIError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("llm api: status %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("llm api: %s", e.Body)
}

// IsConfigError 判断该错误是否属于配置问题（密钥、地址、模型名之类）。
// 这类错误只有让用户看到原文才修得好，任何包装都只会掩盖问题。
func IsConfigError(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Kind == KindConfig {
		return true
	}
	switch ae.Status {
	case 401, 403, 404:
		return true
	}
	return false
}

// IsTransientError 判断一次模型调用失败是否值得换下一个备用模型。
//
// 只放行瞬态故障：限流、服务端错误、请求超时与网络问题。配置类错误、内容拦截、
// 普通 4xx 都不切换——前两类要么需要用户看到原文去修，要么换一家也可能同样被拦；
// 普通 4xx 是请求本身有问题，换模型掩盖不了。全部尝试失败后，错误仍交回现有的
// FailureTranslator 失败收尾（见 internal/agent）。
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}

	var ae *APIError
	if errors.As(err, &ae) {
		if ae.Kind == KindConfig || ae.Kind == KindContentFilter {
			return false
		}
		switch {
		case ae.Status == 408, ae.Status == 429, ae.Status >= 500:
			return true
		}
		if ae.Status == 0 && transientStreamCode(ae.Code) {
			return true
		}
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// 连接被拒、被重置、TLS 握手失败等网络错误。这些在换一个地址后可能恢复。
	var ue *url.Error
	return errors.As(err, &ue)
}

// transientStreamCode 识别两家 SSE 错误帧里表示「服务端暂时不可用」的类型。
// 宁可少认几个，也不把内容拦截、参数错误当成可重试：那会让一轮对话被白白重发。
func transientStreamCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "server_error", "internal_error", "internal_server_error", "api_error",
		"overloaded", "overloaded_error",
		"rate_limit_exceeded", "rate_limit_error",
		"timeout", "timeout_error",
		"service_unavailable", "unavailable",
		"500", "502", "503", "504", "529":
		return true
	}
	return false
}
