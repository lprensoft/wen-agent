// Package modelcfg 管理模型与提供商配置：以 models.json 覆盖层的形式
// 叠加在 config.yaml 之上，支持界面上增删改并热生效，不回写 config.yaml。
package modelcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"wen/internal/config"
	"wen/internal/llm"

	"wen/internal/i18n"
)

// File 是 models.json 的落盘结构。
type File struct {
	Version   int        `json:"version"`
	Providers []Provider `json:"providers"`
	// Removed 是删除坠碑：config.yaml 里仍存在、但已在界面上删掉的提供商名，
	// 防止它们在下次启动时复活。
	Removed []string  `json:"removed_providers,omitempty"`
	Current Selection `json:"current"`
}

type Provider struct {
	Name    string `json:"name"` // 唯一标识，也是界面显示名
	Type    string `json:"type"` // openai_compat / anthropic
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	// Dialect 是思考参数方言（openai_compat 专用），空串等同 deepseek。
	Dialect string `json:"thinking_dialect,omitempty"`
	// PromptCache 是否使用提示词缓存（anthropic 专用；nil = 未设置，按开启算）。
	// 开关存在的理由是它有钱的含义：命中按约十分之一计费，而未命中的写入要多付
	// 约四分之一。连续对话几乎必然命中，但若使用节奏比缓存有效期还慢（消息间隔
	// 常常超过几分钟），那就是一直在付写入的钱、从不读回。
	PromptCache *bool   `json:"prompt_cache,omitempty"`
	Models      []Model `json:"models"`
	// Source 仅出现在响应中："config" 表示该条目来自 config.yaml、尚未被界面接管。
	Source string `json:"source,omitempty"`
}

// Model 的可选字段一律用指针：0 与 0.0 是合法取值，必须与「未设置」区分。
type Model struct {
	ID            string   `json:"id"`             // 传给 API 的模型 id
	Name          string   `json:"name,omitempty"` // 显示名，空则回落显示 ID
	ContextLength *int     `json:"context_length,omitempty"`
	MaxTokens     *int     `json:"max_tokens,omitempty"`
	Thinking      *string  `json:"thinking,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
}

type Selection struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Resolved 是当前选中项与全局默认合并后的最终生效参数。
type Resolved struct {
	ProviderName  string
	Type          string
	BaseURL       string
	APIKey        string
	Dialect       string // 思考参数方言
	PromptCache   bool   // 是否使用提示词缓存（anthropic 专用）
	ModelID       string
	Temperature   float64
	MaxTokens     int
	Thinking      string
	ContextLength int
}

// Store 持有配置，负责校验、热更新与原子落盘。
// own 是 models.json 的内容（界面上改过的部分），对外看到的是它与 config.yaml 的合并视图。
type Store struct {
	mu       sync.RWMutex
	path     string
	own      File       // models.json 的内容
	base     []Provider // config.yaml 的 providers 段（按名排序）
	baseSel  Selection  // config.yaml 的 model.provider / model.name
	defaults config.ModelConfig
}

// NewStore 读取 models.json（可能不存在）并与 config.yaml 合并。
// 不写盘：只有界面上真的改了东西才创建文件。
func NewStore(lang, path string, cfg *config.Config) (*Store, error) {
	s := &Store{
		path:     path,
		base:     baseProviders(cfg),
		baseSel:  Selection{Provider: cfg.Model.Provider, Model: cfg.Model.Name},
		defaults: cfg.Model,
	}

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.own); err != nil {
			return nil, fmt.Errorf(i18n.T(lang, "modelcfg.err.parse", "解析 %s 失败: %w"), path, err)
		}
	case os.IsNotExist(err):
		s.own = File{Version: 1}
	default:
		return nil, fmt.Errorf(i18n.T(lang, "modelcfg.err.read", "读取 %s 失败: %w"), path, err)
	}

	if err := validateDoc(lang, s.viewLocked()); err != nil {
		return nil, fmt.Errorf(i18n.T(lang, "modelcfg.err.invalid", "模型配置无效: %w"), err)
	}
	return s, nil
}

// viewLocked 返回 models.json 与 config.yaml 的合并视图。
func (s *Store) viewLocked() File {
	return mergeWithBase(s.own, s.base, s.baseSel)
}

// baseProviders 把 config.yaml 的 providers（map，无序）转成按名排序的切片，
// 保证列表顺序稳定；每个提供商的模型列表由 model.name 种子决定。
func baseProviders(cfg *config.Config) []Provider {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]Provider, 0, len(names))
	for _, name := range names {
		pc := cfg.Providers[name]
		p := Provider{Name: name, Type: pc.Type, BaseURL: pc.BaseURL, APIKey: pc.APIKey,
			Dialect: pc.ThinkingDialect, PromptCache: pc.PromptCache, Models: []Model{}}
		if name == cfg.Model.Provider && cfg.Model.Name != "" {
			p.Models = append(p.Models, Model{ID: cfg.Model.Name})
		}
		out = append(out, p)
	}
	return out
}

// Snapshot 返回合并后的完整文档（含明文密钥，仅供内部使用）。
func (s *Store) Snapshot() File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.viewLocked()
}

// Defaults 返回 config.yaml model: 段的全局默认值（模型条目留空时的回退来源）。
func (s *Store) Defaults() config.ModelConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.defaults
}

// Resolve 返回当前选中项的最终生效参数。
func (s *Store) Resolve(lang string) (Resolved, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return resolve(lang, s.viewLocked(), s.defaults)
}

// Status 返回 /api/status 需要的非敏感字段。
func (s *Store) Status() (provider, model, thinking string, contextLength int) {
	// 这里的报错被丢弃（拿不到就报空），语言无关紧要
	r, err := s.Resolve("")
	if err != nil {
		return "", "", "", 0
	}
	return r.ProviderName, r.ModelID, r.Thinking, r.ContextLength
}

func resolve(lang string, f File, def config.ModelConfig) (Resolved, error) {
	p, ok := findProvider(f.Providers, f.Current.Provider)
	if !ok {
		return Resolved{}, fmt.Errorf(i18n.T(lang, "modelcfg.err.no_such_provider", "未找到提供商 %q"), f.Current.Provider)
	}
	m, ok := findModel(p.Models, f.Current.Model)
	if !ok {
		return Resolved{}, fmt.Errorf(i18n.T(lang, "modelcfg.err.no_such_model", "提供商 %q 下未找到模型 %q"), p.Name, f.Current.Model)
	}
	r := Resolved{
		ProviderName:  p.Name,
		Type:          p.Type,
		BaseURL:       p.BaseURL,
		APIKey:        p.APIKey,
		Dialect:       p.Dialect,
		PromptCache:   p.PromptCache == nil || *p.PromptCache, // 未设置按开启算
		ModelID:       m.ID,
		Temperature:   def.Temperature,
		MaxTokens:     def.MaxTokens,
		Thinking:      def.Thinking,
		ContextLength: def.ContextLength,
	}
	if m.Temperature != nil {
		r.Temperature = *m.Temperature
	}
	if m.MaxTokens != nil {
		r.MaxTokens = *m.MaxTokens
	}
	if m.Thinking != nil {
		r.Thinking = *m.Thinking
	}
	if m.ContextLength != nil {
		r.ContextLength = *m.ContextLength
	}
	return r, nil
}

// Save 校验并保存整份配置。api_key 为空表示沿用旧值；
// 与 config.yaml 完全一致且从未被接管的提供商不写进 models.json，继续跟随配置文件。
func (s *Store) Save(lang string, next File) (File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	own := carryOverKeys(next, s.viewLocked())
	normalize(&own)
	own.Version = 1
	// 先校验提交的文档本身（重名、非法字段），再算坠碑与「未接管」条目
	if err := validateDoc(lang, own); err != nil {
		return File{}, err
	}
	own.Removed = tombstones(own.Providers, s.base, s.own.Removed)
	own.Providers = dropUnchangedBase(own.Providers, s.base, s.own.Providers)

	prev := s.own
	s.own = own
	view := s.viewLocked()
	// 删除保护：保存后当前使用的提供商与模型必须仍然存在
	if err := checkCurrent(lang, view.Providers, own.Current); err != nil {
		s.own = prev
		return File{}, err
	}
	if err := s.persistLocked(lang); err != nil {
		s.own = prev
		return File{}, err
	}
	return s.viewLocked(), nil
}

// SetCurrent 切换当前使用的提供商与模型。
func (s *Store) SetCurrent(lang string, sel Selection) (File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	view := s.viewLocked()
	p, ok := findProvider(view.Providers, sel.Provider)
	if !ok {
		return File{}, fmt.Errorf(i18n.T(lang, "modelcfg.err.no_such_provider", "未找到提供商 %q"), sel.Provider)
	}
	if _, ok := findModel(p.Models, sel.Model); !ok {
		return File{}, fmt.Errorf(i18n.T(lang, "modelcfg.err.no_such_model", "提供商 %q 下未找到模型 %q"), sel.Provider, sel.Model)
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return File{}, fmt.Errorf(i18n.T(lang, "modelcfg.err.no_api_key", "提供商 %q 尚未配置 API Key，无法使用"), sel.Provider)
	}

	prev := s.own.Current
	s.own.Current = Selection{Provider: p.Name, Model: sel.Model}
	if err := s.persistLocked(lang); err != nil {
		s.own.Current = prev
		return File{}, err
	}
	return s.viewLocked(), nil
}

// Lookup 按名取提供商及其模型（供测试连接使用）。
func (s *Store) Lookup(provider, model string) (Provider, Model, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	view := s.viewLocked()
	p, ok := findProvider(view.Providers, provider)
	if !ok {
		return Provider{}, Model{}, false
	}
	m, ok := findModel(p.Models, model)
	if !ok {
		return p, Model{}, false
	}
	return p, m, true
}

// persistLocked 原子写盘（temp + rename），文件含明文密钥故权限为 0600。
func (s *Store) persistLocked(lang string) error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s.own, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.save", "保存模型配置失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.save", "保存模型配置失败: %w"), err)
	}
	return nil
}

// ---------- 辅助 ----------

func findProvider(ps []Provider, name string) (Provider, bool) {
	for _, p := range ps {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Provider{}, false
}

func findModel(ms []Model, id string) (Model, bool) {
	for _, m := range ms {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

func cloneFile(f File) File {
	out := f
	out.Providers = make([]Provider, len(f.Providers))
	for i, p := range f.Providers {
		p.Models = append([]Model(nil), p.Models...)
		out.Providers[i] = p
	}
	out.Removed = append([]string(nil), f.Removed...)
	return out
}

// TypeOptions 返回界面下拉需要的 API 模式候选。
func TypeOptions() []map[string]string {
	out := make([]map[string]string, 0, len(llm.KnownTypes))
	for _, t := range llm.KnownTypes {
		out = append(out, map[string]string{
			"value":            t,
			"label":            llm.TypeLabel(t),
			"default_base_url": llm.DefaultBaseURL(t),
		})
	}
	return out
}

// DialectOptions 返回界面下拉需要的思考参数方言候选。
func DialectOptions() []map[string]string {
	out := make([]map[string]string, 0, len(llm.KnownDialects))
	for _, d := range llm.KnownDialects {
		out = append(out, map[string]string{"value": d, "label": llm.DialectLabel(d)})
	}
	return out
}
