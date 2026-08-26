package llm

import (
	"fmt"
	"slices"
	"strings"

	"wen/internal/i18n"
)

// Provider 的 API 模式。
const (
	TypeOpenAICompat = "openai_compat"
	TypeAnthropic    = "anthropic"
)

// KnownTypes 是支持的 API 模式（配置校验与界面下拉共用，避免两处漂移）。
var KnownTypes = []string{TypeOpenAICompat, TypeAnthropic}

// TypeLabel 返回 API 模式的显示名。它进设置页的下拉框，按请求语言取。
func TypeLabel(lang, typ string) string {
	switch typ {
	case TypeOpenAICompat:
		return i18n.T(lang, "llm.type.openai_compat", "OpenAI 兼容")
	case TypeAnthropic:
		return i18n.T(lang, "llm.type.anthropic", "Anthropic 兼容")
	}
	return typ
}

// DefaultBaseURL 返回该 API 模式常用的默认地址，供界面预填。
func DefaultBaseURL(typ string) string {
	switch typ {
	case TypeOpenAICompat:
		return "https://api.deepseek.com"
	case TypeAnthropic:
		return "https://api.anthropic.com"
	}
	return ""
}

func IsKnownType(typ string) bool { return slices.Contains(KnownTypes, typ) }

// Config 是创建 Provider 所需的提供商级参数。用结构体而不是一串位置参数：
// 后面两项都是「只对某一种 API 模式有意义」的可选项，位置传参很容易接错。
type Config struct {
	Type    string
	BaseURL string
	APIKey  string
	// Dialect 是思考参数方言（见 dialect.go），仅 openai_compat 使用；
	// 空串等同 DeepSeek 方言。
	Dialect string
	// Lang 是这个 provider 说话用的语言：它产生的报错会经会话进到对话界面。
	// 取**进程**语言而不是某次请求的——provider 是进程级单实例，一次英文请求
	// 建出来的它，之后要为每个人报错。
	Lang string
	// PromptCache 是否使用提示词缓存，仅 anthropic 使用——它要求调用方在请求里
	// 显式打断点，且缓存写入单独计费。openai_compat 那边（DeepSeek、OpenAI）的
	// 缓存由服务端自动维护，与这个开关无关，命中与否只取决于前缀是否稳定。
	PromptCache bool
}

// New 按 API 模式创建 Provider。
func New(cfg Config) (Provider, error) {
	if !IsKnownDialect(cfg.Dialect) {
		return nil, fmt.Errorf(i18n.T(cfg.Lang, "llm.err.bad_dialect",
			"不支持的思考参数方言 %q（可选：%s）"), cfg.Dialect, strings.Join(KnownDialects, " / "))
	}
	switch cfg.Type {
	case TypeOpenAICompat:
		return NewOpenAICompat(cfg.Lang, cfg.BaseURL, cfg.APIKey, cfg.Dialect), nil
	case TypeAnthropic:
		return NewAnthropic(cfg.Lang, cfg.BaseURL, cfg.APIKey, cfg.PromptCache), nil
	}
	return nil, fmt.Errorf(i18n.T(cfg.Lang, "llm.err.bad_type",
		"不支持的 API 模式 %q（可选：%s）"), cfg.Type, strings.Join(KnownTypes, " / "))
}
