package modelcfg

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"wen/internal/llm"

	"wen/internal/i18n"
)

// thinkingLevels 与 config.validate 保持一致。
var thinkingLevels = []string{"off", "low", "medium", "high", "xhigh", "max"}

const providerNameMaxLen = 64

// validateDoc 校验提供商与模型本身，错误文案面向界面直接展示。
func validateDoc(lang string, f File) error {
	if len(f.Providers) == 0 {
		return fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.no_provider", "至少需要保留一个提供商"))
	}
	seen := map[string]bool{}
	for _, p := range f.Providers {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.empty_name", "提供商名称不能为空"))
		}
		if len([]rune(name)) > providerNameMaxLen {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.name_long", "提供商名称 %q 过长（最多 %d 个字符）"), name, providerNameMaxLen)
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.dup_name", "提供商名称 %q 重复"), name)
		}
		seen[key] = true

		if !llm.IsKnownType(p.Type) {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.bad_type", "提供商 %q 的 API 模式 %q 不支持（可选：%s）"),
				name, p.Type, strings.Join(llm.KnownTypes, " / "))
		}
		if !llm.IsKnownDialect(p.Dialect) {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.bad_dialect", "提供商 %q 的思考参数方言 %q 不支持（可选：%s）"),
				name, p.Dialect, strings.Join(llm.KnownDialects, " / "))
		}
		if err := validateBaseURL(lang, p.BaseURL); err != nil {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.provider_wrap", "提供商 %q 的 %w"), name, err)
		}
		if err := validateModels(lang, name, p.Models); err != nil {
			return err
		}
	}

	return nil
}

// checkCurrent 确认当前使用的提供商与模型在保存后依然存在（删除保护）。
func checkCurrent(lang string, ps []Provider, want Selection) error {
	if want.Provider == "" || want.Model == "" {
		return nil
	}
	p, ok := findProvider(ps, want.Provider)
	if !ok {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.provider_in_use", "提供商 %q 正在使用中，请先切换到其他模型再删除"), want.Provider)
	}
	if _, ok := findModel(p.Models, want.Model); !ok {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.model_in_use", "模型 %q 正在使用中，请先切换到其他模型再删除"), want.Model)
	}
	return nil
}

func validateBaseURL(lang, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.bad_base_url", "Base URL 格式不正确，需以 http:// 或 https:// 开头"))
	}
	return nil
}

func validateModels(lang, provider string, ms []Model) error {
	seen := map[string]bool{}
	for _, m := range ms {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.empty_model_id", "提供商 %q 下的模型 ID 不能为空"), provider)
		}
		if seen[id] {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.dup_model", "模型 ID %q 在提供商 %q 下重复"), id, provider)
		}
		seen[id] = true

		if m.Thinking != nil && !slices.Contains(thinkingLevels, *m.Thinking) {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.bad_thinking", "模型 %q 的思考模式 %q 无效（%s）"), id, *m.Thinking, strings.Join(thinkingLevels, "/"))
		}
		if m.Temperature != nil && (*m.Temperature < 0 || *m.Temperature > 2) {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.bad_temperature", "模型 %q 的 temperature 需在 0 ~ 2 之间"), id)
		}
		if m.MaxTokens != nil && *m.MaxTokens <= 0 {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.bad_max_tokens", "模型 %q 的最大输出 tokens 需大于 0"), id)
		}
		if m.ContextLength != nil && *m.ContextLength <= 0 {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.bad_context", "模型 %q 的上下文窗口需大于 0"), id)
		}
	}
	return nil
}

// RequiresKey 回答「这个地址要不要 API Key」。
//
// 判据是 base_url 指不指向本机：Ollama、LM Studio、llama.cpp、本地 vLLM 都不做鉴权，
// OpenAI SDK 那边填的是个任意占位串。之所以按地址判而不是给提供商加一个「无需密钥」
// 开关，是因为那个开关会出现在设置页上，而它对着一个远端地址勾上就是一次误配；
// 地址本身已经把这件事说清楚了，不需要用户再声明一遍。
func RequiresKey(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return true
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return false
	}
	return true
}

// MaskKey 把 API Key 掩码成可安全展示的形式。
func MaskKey(key string) string {
	if key == "" {
		return ""
	}
	if len([]rune(key)) <= 8 {
		return "••••••••"
	}
	r := []rune(key)
	return string(r[:4]) + "••••••••" + string(r[len(r)-4:])
}
