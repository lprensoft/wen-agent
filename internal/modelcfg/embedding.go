package modelcfg

import (
	"fmt"
	"strings"

	"wen/internal/i18n"
	"wen/internal/llm"
)

// 嵌入服务的配置。它与聊天提供商是两套东西（见 llm/embed.go），但同样落在 models.json：
// 都是「连哪家、用什么密钥」，含明文密钥，权限与保存方式一致；放进插件配置的话，
// 每个要用向量的插件都得各配一遍。
//
// 默认关闭，且必须由用户显式选择提供商并打开：启用后，记忆正文与每轮的用户输入
// 会发给这家服务，这是个必须看得见的决定。

// Embedding 是嵌入服务的配置。零值就是「关」。
type Embedding struct {
	// Enabled 是显式授权。其余字段填好而这里为假，等于没配——配置可以先留着，
	// 临时关掉不必重填。
	Enabled bool   `json:"enabled"`
	Type    string `json:"type"` // openai_compat / ollama
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
	// Dimensions 是请求的向量维度，0 表示用模型默认值。
	Dimensions int `json:"dimensions,omitempty"`
}

// maxEmbedDimensions 是维度的上限。主流模型最大不过 4096，留一档余量；
// 上限存在是为了挡住手误（多敲几个 0），而不是什么技术限制。
const maxEmbedDimensions = 8192

// LLMConfig 转成构造 Embedder 的参数。
func (e Embedding) LLMConfig(lang string) llm.EmbedConfig {
	return llm.EmbedConfig{
		Type: e.Type, BaseURL: e.BaseURL, APIKey: e.APIKey,
		Model: e.Model, Dimensions: e.Dimensions, Lang: lang,
	}
}

// Active 回答「现在要不要用嵌入」：显式开启且配置完整。
func (e Embedding) Active() bool {
	return e.Enabled && llm.IsKnownEmbedType(e.Type) && e.BaseURL != "" && e.Model != ""
}

// NormalizeEmbedding 规整用户输入：去空白、去地址末尾斜杠。
func NormalizeEmbedding(e Embedding) Embedding {
	e.Type = strings.TrimSpace(e.Type)
	e.BaseURL = strings.TrimRight(strings.TrimSpace(e.BaseURL), "/")
	e.APIKey = strings.TrimSpace(e.APIKey)
	e.Model = strings.TrimSpace(e.Model)
	return e
}

// ValidateEmbedding 校验嵌入配置，错误文案面向界面直接展示。
// 关着的配置只校验已经填了的部分：留一份写到一半的配置不该被挡住。
func ValidateEmbedding(lang string, e Embedding) error {
	if e.Type != "" && !llm.IsKnownEmbedType(e.Type) {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.embed_bad_type", "嵌入接口 %q 不支持（可选：%s）"),
			e.Type, strings.Join(llm.KnownEmbedTypes, " / "))
	}
	if e.BaseURL != "" {
		if err := validateBaseURL(lang, e.BaseURL); err != nil {
			return err
		}
	}
	if e.Dimensions < 0 || e.Dimensions > maxEmbedDimensions {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.embed_bad_dims", "向量维度需在 0 ~ %d 之间（0 表示用模型默认值）"), maxEmbedDimensions)
	}
	if !e.Enabled {
		return nil
	}
	if !llm.IsKnownEmbedType(e.Type) {
		return fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.embed_no_type", "请选择嵌入接口"))
	}
	if e.BaseURL == "" {
		return fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.embed_no_base_url", "请填写嵌入服务地址"))
	}
	if e.Model == "" {
		return fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.embed_no_model", "请填写嵌入模型"))
	}
	if e.APIKey == "" && RequiresKey(e.BaseURL) {
		return fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.embed_no_key", "请填写嵌入服务的 API Key"))
	}
	return nil
}

// InheritEmbeddingKey 把留空的密钥用已保存的补上（界面只拿得到掩码）。
// 只在地址没变时沿用：地址换了还带着旧密钥，等于把它发给另一个主人。
func InheritEmbeddingKey(next, saved Embedding) Embedding {
	if next.APIKey == "" && strings.EqualFold(
		strings.TrimRight(strings.TrimSpace(next.BaseURL), "/"),
		strings.TrimRight(strings.TrimSpace(saved.BaseURL), "/")) {
		next.APIKey = saved.APIKey
	}
	return next
}

// Embedding 返回已保存的嵌入配置（含明文密钥，仅供内部使用）。
func (s *Store) Embedding() Embedding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.own.Embedding == nil {
		return Embedding{}
	}
	return *s.own.Embedding
}

// SetEmbedding 校验并保存嵌入配置。api_key 为空且地址没变时沿用旧值。
func (s *Store) SetEmbedding(lang string, next Embedding) (Embedding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var saved Embedding
	if s.own.Embedding != nil {
		saved = *s.own.Embedding
	}
	next = NormalizeEmbedding(next)
	next = InheritEmbeddingKey(next, saved)
	if err := ValidateEmbedding(lang, next); err != nil {
		return Embedding{}, err
	}

	prev := s.own.Embedding
	if next == (Embedding{}) {
		s.own.Embedding = nil // 全空就别在文件里留一段空壳
	} else {
		s.own.Embedding = &next
	}
	if err := s.persistLocked(lang); err != nil {
		s.own.Embedding = prev
		return Embedding{}, err
	}
	return next, nil
}

// EmbeddingPreset 是一条预置的嵌入服务，只用来填表单，保存下来的是一条普通配置。
// 与聊天提供商的预置同理（见 presets.go）：名字不翻译，会存进用户的认知里当作标识。
type EmbeddingPreset struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
}

// embeddingPresets 里的地址与模型 ID 取自各家文档，但还没有逐家用真实密钥打过
// 一遍（见 docs/decisions/external-protocol-verification.md 的待验清单）；
// 发版时随聊天预置一起复核。顺序：国内常用的在前，本地压末尾。
var embeddingPresets = []EmbeddingPreset{
	{Key: "siliconflow_cn", Name: "SiliconFlow (CN)", Type: llm.EmbedTypeOpenAI,
		BaseURL: "https://api.siliconflow.cn/v1", Model: "BAAI/bge-m3"},
	{Key: "qwen_cn", Name: "Qwen (CN)", Type: llm.EmbedTypeOpenAI,
		BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "text-embedding-v4"},
	{Key: "zai_cn", Name: "Z.AI (CN)", Type: llm.EmbedTypeOpenAI,
		BaseURL: "https://open.bigmodel.cn/api/paas/v4", Model: "embedding-3"},
	{Key: "openai", Name: "OpenAI", Type: llm.EmbedTypeOpenAI,
		BaseURL: "https://api.openai.com/v1", Model: "text-embedding-3-small"},
	{Key: "voyage", Name: "Voyage AI", Type: llm.EmbedTypeOpenAI,
		BaseURL: "https://api.voyageai.com/v1", Model: "voyage-4"},
	// bge-m3 多语言，中文表现好；要先 ollama pull bge-m3
	{Key: "ollama", Name: "Ollama", Type: llm.EmbedTypeOllama,
		BaseURL: "http://localhost:11434", Model: "bge-m3"},
}

// EmbeddingPresets 返回预置表的副本。
func EmbeddingPresets() []EmbeddingPreset {
	return append([]EmbeddingPreset(nil), embeddingPresets...)
}

// EmbedTypeOptions 返回界面下拉需要的嵌入接口候选。
func EmbedTypeOptions(lang string) []map[string]string {
	out := make([]map[string]string, 0, len(llm.KnownEmbedTypes))
	for _, t := range llm.KnownEmbedTypes {
		out = append(out, map[string]string{"value": t, "label": llm.EmbedTypeLabel(lang, t)})
	}
	return out
}
