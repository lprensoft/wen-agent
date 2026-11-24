package modelcfg

import (
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"wen/internal/config"
	"wen/internal/llm"

	"wen/internal/i18n"
)

// thinkingLevels 与 config.validate 保持一致。
var thinkingLevels = []string{"off", "low", "medium", "high", "xhigh", "max"}

const providerNameMaxLen = 64

// priceCurrencyMaxLen 是币种标识的长度上限。ISO 4217 的代码是 3 位，留到 8 位是给
// 「USD/million」这类用户自己写的符号余地；再长就不是币种而是一句话了。
const priceCurrencyMaxLen = 8

// dupCheck 决定重名怎么判：保存用当前的查找口径（EqualFold），载入还要兼容旧文件
// 与新版本自己写出的文件（见 validateLoadedDoc）。
type dupCheck int

const (
	dupEqualFold dupCheck = iota // 保存：只差大小写即视为同一家
	dupBothRules                 // 载入：旧版 ToLower 与当前 EqualFold 都认为重复才拒绝
)

// validateDoc 校验提供商与模型本身，错误文案面向界面直接展示。保存走这一条：
// 重名按 EqualFold 一律拒绝。
func validateDoc(lang string, f File) error { return validateProviders(lang, f, dupEqualFold) }

// validateLoadedDoc 是载入 models.json 与 config.yaml 时的校验：除重名的判法外与
// validateDoc 相同。
//
// 载入既不能照搬保存时的 EqualFold，也不能完全关掉重名检查：
//   - 旧版本按 ToLower 判重，曾接受「Ζευς」/「ΖΕΥΣ」（ToLower 折成两串，EqualFold
//     却视为同一个）。这类旧文件必须能进设置页改名，否则用户连修正的机会都没有。
//   - 两个都叫「Alpha」的条目旧版就会拒绝，只可能来自手改；按名字查找只会静默命中
//     第一家，载入时该报错，而不是让运行时选一个。
//   - 新版本保存允许 ToLower 相同、EqualFold 不同（如 İstanbul/istanbul），载入也
//     必须接受，否则刚保存成功的配置会起不来。
//
// 交集正好满足三条：旧版 ToLower 与当前 EqualFold 都认为重复时才拒绝。
func validateLoadedDoc(lang string, f File) error { return validateProviders(lang, f, dupBothRules) }

func validateProviders(lang string, f File, dup dupCheck) error {
	if len(f.Providers) == 0 {
		return fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.no_provider", "至少需要保留一个提供商"))
	}
	// 重名按 EqualFold 判，与查找口径一致（findProvider）：只差大小写的两个名字在
	// 查价、选模型时是同一个，同时存在会让「哪一家生效」取决于列表顺序。只折小写
	// 不够：希腊字母的 σ 与词尾 ς 折叠后仍不同串，而 EqualFold 认它们是同一个。
	var seen []string
	for _, p := range f.Providers {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.empty_name", "提供商名称不能为空"))
		}
		if len([]rune(name)) > providerNameMaxLen {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.name_long", "提供商名称 %q 过长（最多 %d 个字符）"), name, providerNameMaxLen)
		}
		for _, prev := range seen {
			if !strings.EqualFold(prev, name) {
				continue
			}
			if dup == dupEqualFold || strings.ToLower(prev) == strings.ToLower(name) {
				return fmt.Errorf(i18n.T(lang, "modelcfg.err.dup_name", "提供商名称 %q 重复"), name)
			}
		}
		seen = append(seen, name)

		if !llm.IsKnownType(p.Type) {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.bad_type", "提供商 %q 的 API 模式 %q 不支持（可选：%s）"),
				name, p.Type, strings.Join(llm.KnownTypes, " / "))
		}
		if !llm.IsKnownDialect(p.Dialect) {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.bad_dialect", "提供商 %q 的思考参数方言 %q 不支持（可选：%s）"),
				name, p.Dialect, strings.Join(llm.KnownDialects, " / "))
		}
		if !llm.IsKnownProtocol(p.Protocol) {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.bad_protocol", "提供商 %q 的线上协议 %q 不支持（可选：%s）"),
				name, p.Protocol, strings.Join(llm.KnownProtocols, " / "))
		}
		if p.Protocol == llm.ProtocolResponses && p.Type != llm.TypeOpenAICompat {
			return fmt.Errorf(i18n.T(lang, "modelcfg.err.protocol_type", "提供商 %q 的线上协议 responses 只能用于 %s"),
				name, llm.TypeOpenAICompat)
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

// validateFallbacks 确认每个备用模型都指向真实存在、且具备可用密钥的组合。
// 传入合并后的视图：config.yaml 里的提供商同样可以当备用模型。
func validateFallbacks(lang string, f File) error {
	for i, sel := range f.Fallbacks {
		if sel.Provider == "" || sel.Model == "" {
			return fmt.Errorf("%s", i18n.Tf(lang, "modelcfg.err.fallback_incomplete", "第 %d 个备用模型不完整", i+1))
		}
		if _, err := resolveSelection(lang, f, config.ModelConfig{}, sel); err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "modelcfg.err.fallback_resolve", "备用模型 %d（%s / %s）：%v", i+1, sel.Provider, sel.Model, err))
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

// CheckBaseURL 是保存时那条 Base URL 校验的对外入口：首次运行向导在表单上就用它把关，
// 不必等到保存才报错。
func CheckBaseURL(lang, raw string) error {
	return validateBaseURL(lang, raw)
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
		if err := validatePrice(lang, id, m); err != nil {
			return err
		}
	}
	return nil
}

// validatePrice 校验价格表里的一项。币种是金额的唯一归属（账本按它分桶），空串会让
// 那笔钱落进「未定价」；中间夹空白则会让同一个币种在合计里裂成两行。负数金额没有
// 意义，多半是把小数点点错了。
//
// NaN 与 ±Inf 也要挡：它们既不是价格，也写不进 JSON，账本按这种单价算出的金额会
// 一路是 Inf，最后让整份用量报告编码失败（状态码 200、响应体空）。有限但极大的值
// （如 1e308）照旧放行——那是用户的自由，金额一侧饱和即可（见 usage.Price.Cost）。
func validatePrice(lang, id string, m Model) error {
	if m.Price == nil {
		return nil
	}
	currency := strings.TrimSpace(m.Price.Currency)
	if currency == "" || len([]rune(currency)) > priceCurrencyMaxLen ||
		strings.ContainsFunc(currency, unicode.IsSpace) {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.price_currency",
			"模型 %q 的价格币种 %q 无效（需 1 ~ 8 个字符，且不含空白）"), id, currency)
	}
	if !finitePrice(m.Price.Input) || !finitePrice(m.Price.Output) ||
		(m.Price.CacheRead != nil && !finitePrice(*m.Price.CacheRead)) ||
		(m.Price.CacheWrite != nil && !finitePrice(*m.Price.CacheWrite)) {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.price_finite",
			"模型 %q 的价格必须是有限数（不能是 NaN 或无穷大）"), id)
	}
	if m.Price.Input < 0 || m.Price.Output < 0 ||
		(m.Price.CacheRead != nil && *m.Price.CacheRead < 0) ||
		(m.Price.CacheWrite != nil && *m.Price.CacheWrite < 0) {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.price_negative", "模型 %q 的价格不能为负数"), id)
	}
	return nil
}

// finitePrice 判断一个价格项是不是有限的数。
func finitePrice(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

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
