package llm

import (
	"errors"
	"fmt"
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
	Body   string // 错误体原文（调用方已截断）
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
