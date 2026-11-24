// Package modelcfg 管理模型与提供商配置：以 models.json 覆盖层的形式
// 叠加在 config.yaml 之上，支持界面上增删改并热生效，不回写 config.yaml。
package modelcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"wen/internal/atomicfile"
	"wen/internal/config"
	"wen/internal/llm"
	"wen/internal/usage"

	"wen/internal/i18n"
)

// File 是 models.json 的落盘结构。
type File struct {
	Version   int        `json:"version"`
	Providers []Provider `json:"providers"`
	// Removed 是删除坠碑：config.yaml 里仍存在、但已在界面上删掉的提供商名，
	// 防止它们在下次启动时复活。
	Removed []string `json:"removed_providers,omitempty"`
	// Fallbacks 是主模型不可用时按顺序尝试的备用模型。只在运行期瞬态错误
	// （限流、服务端故障、网络问题）时切换，见 internal/agent。
	Fallbacks []Selection `json:"fallbacks,omitempty"`
	Current   Selection   `json:"current"`
	// Embedding 是嵌入服务配置，见 embedding.go。它不属于「整档提交」的范围：
	// Save 沿用已保存的值，只有 SetEmbedding 能改它，否则每次保存提供商都会把它抹掉。
	Embedding *Embedding `json:"embedding,omitempty"`
}

type Provider struct {
	Name    string `json:"name"` // 唯一标识，也是界面显示名
	Type    string `json:"type"` // openai_compat / anthropic
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	// Dialect 是思考参数方言（openai_compat 专用），空串等同 deepseek。
	Dialect string `json:"thinking_dialect,omitempty"`
	// Protocol 是 openai_compat 的线上协议：空串等同 chat（/chat/completions），
	// responses 走 /v1/responses。anthropic 不使用。
	Protocol string `json:"protocol,omitempty"`
	// PromptCache 是否使用提示词缓存（anthropic 专用；nil = 未设置，按开启算）。
	// 开关存在的理由是它有钱的含义：命中按约十分之一计费，而未命中的写入要多付
	// 约四分之一。连续对话几乎必然命中，但若使用节奏比缓存有效期还慢（消息间隔
	// 常常超过几分钟），那就是一直在付写入的钱、从不读回。
	PromptCache *bool   `json:"prompt_cache,omitempty"`
	Models      []Model `json:"models"`
	// Source 仅出现在响应中："config" 表示该条目来自 config.yaml、尚未被界面接管。
	Source string `json:"source,omitempty"`
	// Ref 是界面往返用的原始标识，同样只出现在响应里、不落盘（保存时由 normalize
	// 清掉）。旧版本允许只差大小写的两个提供商共存，改名保存时按名字回填密钥会被
	// EqualFold 带到另一家；界面把 Ref 原样带回，保存时据此找到被编辑的那一条，
	// 见 carryOverKeys。
	Ref string `json:"ref,omitempty"`
}

// Model 的可选字段一律用指针：0 与 0.0 是合法取值，必须与「未设置」区分。
type Model struct {
	ID            string   `json:"id"`             // 传给 API 的模型 id
	Name          string   `json:"name,omitempty"` // 显示名，空则回落显示 ID
	ContextLength *int     `json:"context_length,omitempty"`
	MaxTokens     *int     `json:"max_tokens,omitempty"`
	Thinking      *string  `json:"thinking,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	// Price 是单价（每百万 token），供用量账本把 token 折成钱。nil 表示没定价，
	// 这一点与全 0 的价格不同：没定价的用量计成「未定价 token」，不会悄悄算成 0 元。
	// 价格只存在 models.json 里，config.yaml 那份配置没有模型条目、也就没有价格。
	Price *usage.Price `json:"price,omitempty"`
}

type Selection struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// LLMConfig 把解析结果转成构造 Provider 所需的参数。
func (r Resolved) LLMConfig(lang string) llm.Config {
	return llm.Config{
		Type:        r.Type,
		BaseURL:     r.BaseURL,
		APIKey:      r.APIKey,
		Dialect:     r.Dialect,
		Protocol:    r.Protocol,
		PromptCache: r.PromptCache,
		Lang:        lang,
	}
}

// Resolved 是当前选中项与全局默认合并后的最终生效参数。
type Resolved struct {
	ProviderName  string
	Type          string
	BaseURL       string
	APIKey        string
	Dialect       string // 思考参数方言
	Protocol      string // 线上协议（openai_compat 专用；空串等同 chat）
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

	// 一个提供商都没有是合法的初始状态（全新安装、尚未跑过向导），载入时放行；
	// 能不能用由 Resolve 回答，「至少保留一个」那条只在保存时把关。
	//
	// 载入用宽松版校验：旧文件里可能存着升级后才算重名的提供商（见 validateLoadedDoc），
	// 在进设置页之前就失败会让用户无法改名修正。
	if view := s.viewLocked(); len(view.Providers) > 0 {
		if err := validateLoadedDoc(lang, view); err != nil {
			return nil, fmt.Errorf(i18n.T(lang, "modelcfg.err.invalid", "模型配置无效: %w"), err)
		}
		if err := validateFallbacks(lang, view); err != nil {
			return nil, fmt.Errorf(i18n.T(lang, "modelcfg.err.invalid", "模型配置无效: %w"), err)
		}
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
			Dialect: pc.ThinkingDialect, Protocol: pc.Protocol, PromptCache: pc.PromptCache, Models: []Model{}}
		if name == cfg.Model.Provider && cfg.Model.Name != "" {
			p.Models = append(p.Models, Model{ID: cfg.Model.Name})
		}
		out = append(out, p)
	}
	return out
}

// Snapshot 返回合并后的完整文档（含明文密钥，仅供内部使用）。提供商带上 Ref：
// 它是界面往返时的原始标识，不落盘（见 Provider.Ref）。
func (s *Store) Snapshot() File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.viewLocked()
	for i := range f.Providers {
		f.Providers[i].Ref = providerRef(f.Providers, i)
	}
	return f
}

// providerRef 给列表里的第 i 个提供商生成原始标识：精确名字 + 同名出现序号。
//
// 不用下标：列表增删、改名都不该让未动的条目错位。只用名字也不够：手工文件里可能
// 有完全同名的两条（载入会拒绝，但标识仍应自洽）。名字 + 序号对这两种情况都稳定。
func providerRef(ps []Provider, i int) string {
	ordinal := 0
	for j := 0; j < i; j++ {
		if ps[j].Name == ps[i].Name {
			ordinal++
		}
	}
	return ps[i].Name + "\x1f" + strconv.Itoa(ordinal)
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

// ResolveChain 返回主模型与按顺序排列的备用模型。重复项会被去掉。
func (s *Store) ResolveChain(lang string) ([]Resolved, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.viewLocked()
	primary, err := resolve(lang, f, s.defaults)
	if err != nil {
		return nil, err
	}
	out := []Resolved{primary}
	seen := map[string]bool{selectionKey(primary): true}
	for i, sel := range f.Fallbacks {
		r, err := resolveSelection(lang, f, s.defaults, sel)
		if err != nil {
			return nil, fmt.Errorf("%s", i18n.Tf(lang, "modelcfg.err.fallback_resolve", "备用模型 %d（%s / %s）：%v", i+1, sel.Provider, sel.Model, err))
		}
		key := selectionKey(r)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out, nil
}

// selectionKey 是备用链去重用的键：解析后的实际提供商名与模型 ID，原样拼接。
//
// 不再折小写：两家提供商可以合法地只差「ToLower 相同、EqualFold 不同」的大小写
// （如 İstanbul 与 istanbul），各自都有同名模型时它们是两个不同的目标；折小写会把
// 备用项误判成主模型的重复项丢掉，主模型故障时因此不会切换。同一家换个大小写写进
// 备用项的，resolveSelection 已经返回条目里的实际名字（findProvider 按 EqualFold
// 匹配），键仍然相同，照旧去重。
func selectionKey(r Resolved) string {
	return r.ProviderName + "\x00" + r.ModelID
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

// resolve 也是「这套配置能不能用」的唯一判据：服务启动前据此决定要不要进首次运行
// 向导。缺密钥因此也在这里报——此前它由 config.yaml 那一层查，查在合并 models.json
// 之前，于是只看得见一半配置。
func resolve(lang string, f File, def config.ModelConfig) (Resolved, error) {
	// 一个都没有时报「没配置」，而不是下面那句「未找到提供商 ""」。
	if len(f.Providers) == 0 {
		return Resolved{}, fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.not_configured", "还没有配置模型"))
	}
	return resolveSelection(lang, f, def, f.Current)
}

func resolveSelection(lang string, f File, def config.ModelConfig, sel Selection) (Resolved, error) {
	if sel.Provider == "" {
		return Resolved{}, fmt.Errorf("%s", i18n.T(lang, "modelcfg.err.not_configured", "还没有配置模型"))
	}
	p, ok := findProvider(f.Providers, sel.Provider)
	if !ok {
		return Resolved{}, fmt.Errorf(i18n.T(lang, "modelcfg.err.no_such_provider", "未找到提供商 %q"), sel.Provider)
	}
	m, ok := findModel(p.Models, sel.Model)
	if !ok {
		return Resolved{}, fmt.Errorf(i18n.T(lang, "modelcfg.err.no_such_model", "提供商 %q 下未找到模型 %q"), p.Name, sel.Model)
	}
	if strings.TrimSpace(p.APIKey) == "" && RequiresKey(p.BaseURL) {
		return Resolved{}, fmt.Errorf(i18n.T(lang, "modelcfg.err.no_api_key", "提供商 %q 尚未配置 API Key，无法使用"), p.Name)
	}
	r := Resolved{
		ProviderName:  p.Name,
		Type:          p.Type,
		BaseURL:       p.BaseURL,
		APIKey:        p.APIKey,
		Dialect:       p.Dialect,
		Protocol:      p.Protocol,
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

	own, err := carryOverKeys(lang, next, s.viewLocked())
	if err != nil {
		return File{}, err
	}
	normalize(&own)
	own.Version = 1
	own.Embedding = s.own.Embedding
	// 先校验提交的文档本身（重名、非法字段），再算坠碑与「未接管」条目
	if err := validateDoc(lang, own); err != nil {
		return File{}, err
	}
	own.Removed = tombstones(own.Providers, s.base, s.own.Removed)
	own.Providers = dropUnchangedBase(own.Providers, s.base, s.own.Providers)

	// 备用模型可以指向 config.yaml 里的提供商，因此要在合并后的视图上校验。
	view := mergeWithBase(own, s.base, s.baseSel)
	if err := validateFallbacks(lang, view); err != nil {
		return File{}, err
	}

	prev := s.own
	s.own = own
	view = s.viewLocked()
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
	if strings.TrimSpace(p.APIKey) == "" && RequiresKey(p.BaseURL) {
		return File{}, fmt.Errorf(i18n.T(lang, "modelcfg.err.no_api_key", "提供商 %q 尚未配置 API Key，无法使用"), sel.Provider)
	}

	prev := s.own.Current
	prevFallbacks := append([]Selection(nil), s.own.Fallbacks...)
	s.own.Current = Selection{Provider: p.Name, Model: sel.Model}
	// 把刚升为主模型的组合从备用链里去掉，避免设置页出现重复项。
	s.own.Fallbacks = removeSelection(s.own.Fallbacks, s.own.Current)
	if err := s.persistLocked(lang); err != nil {
		s.own.Current = prev
		s.own.Fallbacks = prevFallbacks
		return File{}, err
	}
	return s.viewLocked(), nil
}

func removeSelection(list []Selection, want Selection) []Selection {
	out := list[:0]
	for _, sel := range list {
		if strings.EqualFold(sel.Provider, want.Provider) && sel.Model == want.Model {
			continue
		}
		out = append(out, sel)
	}
	return out
}

// Lookup 按名取提供商及其模型，供测试连接沿用已保存的密钥与协议。
//
// 名字按 EqualFold 找（与 findProvider 同口径），命中多家时必须带 Ref 才认得出是
// 哪一家：旧配置里两家只差大小写、模型 ID 又相同时，取第一家会把它的密钥发往第二家
// 的地址。带了 Ref 就只认 Ref——对不上说明页面是旧的，同样在发请求之前拒绝。
// ok 为 false 表示没有这个名字的条目（新增的提供商），调用方按请求里的密钥测。
func (s *Store) Lookup(lang, ref, provider, model string) (Provider, Model, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps := s.viewLocked().Providers

	at := -1
	switch {
	case ref != "":
		for i, p := range ps {
			if providerRef(ps, i) == ref && strings.EqualFold(p.Name, provider) {
				at = i
				break
			}
		}
		if at < 0 {
			return Provider{}, Model{}, false, fmt.Errorf(i18n.T(lang, "modelcfg.err.stale_ref",
				"提供商 %q 与当前配置对不上：配置已在别处被修改，请刷新后重试"), provider)
		}
	default:
		for i, p := range ps {
			if !strings.EqualFold(p.Name, provider) {
				continue
			}
			if at >= 0 {
				return Provider{}, Model{}, false, fmt.Errorf(i18n.T(lang, "modelcfg.err.test_ambiguous",
					"有多个提供商都叫 %q（只差大小写），无法确定要沿用哪一家的密钥；请刷新页面后重试"), provider)
			}
			at = i
		}
	}
	if at < 0 {
		return Provider{}, Model{}, false, nil
	}
	p := ps[at]
	m, ok := findModel(p.Models, model)
	if !ok {
		return Provider{}, Model{}, false, nil
	}
	return p, m, true, nil
}

// Price 返回某提供商下某个模型的单价，供用量账本估算金额。查的是 config.yaml +
// models.json 的合并视图：界面接管过的提供商（含价格）优先，未接管的仍按配置文件
// 里的名字可见——同一对 (提供商, 模型) 在哪一侧定义都该查到同一份价格。
//
// 没填价格（或提供商/模型不存在）返回 false，而不是零值 Price：零值代表的是一份
// 「0 元」的定价，会把没定价的用量悄悄算成免费。
func (s *Store) Price(provider, model string) (usage.Price, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := findProvider(s.viewLocked().Providers, provider)
	if !ok {
		return usage.Price{}, false
	}
	m, ok := findModel(p.Models, model)
	if !ok || m.Price == nil {
		return usage.Price{}, false
	}
	return *m.Price, true
}

// priceEntry 是价格快照里的一个提供商：名字与它下面每个模型的单价按合并视图的
// 顺序复制一份。查询时先选中第一个名字等价的提供商，再在它下面找模型——顺序与
// Price 的 findProvider → findModel 完全一致。
type priceEntry struct {
	provider string
	models   []priceModel
}

type priceModel struct {
	id    string
	price *usage.Price // nil 表示没定价
}

// clonePrice 复制一份单价。CacheRead / CacheWrite 是指针，浅拷贝只会共享同一块内存；
// 快照要的是一份在 Store 之后怎么改都不受影响的值，所以这里把指针指向的数也复制出来。
func clonePrice(p usage.Price) usage.Price {
	out := p
	if p.CacheRead != nil {
		v := *p.CacheRead
		out.CacheRead = &v
	}
	if p.CacheWrite != nil {
		v := *p.CacheWrite
		out.CacheWrite = &v
	}
	return out
}

// PriceSnapshot 一次性取回价格表的只读快照，返回的闭包可以放心传给整月扫描。
//
// 报告页扫一整个月的账本时，价格是每条记录现查的（见 Price）。扫描期间若在界面上
// 保存了一次模型配置，同一份报表就会前半截旧价、后半截新价——合计与分组、甚至同一天
// 的两条记录各说各话，而它们本该出自同一份价格表。这里在锁内遍历一次合并视图
// （config.yaml + models.json），把提供商与模型按原顺序复制进一份切片；闭包只读这份
// 切片，调用期间不再碰 Store，也就不再受后续保存影响。
//
// 查找口径与 Price 逐字一致：先按 strings.EqualFold 选中第一个同名提供商
// （findProvider 同一口径），再在它下面按模型 ID 精确匹配；第一家具名就是答案，
// 没这个模型或没定价都返回 false。不能「按模型扫全表」——两家名字只差大小写、
// 第一家未定价时，那会跳过第一家取到第二家的价，报表与实时预算因此对不上。
//
// 提供商名不用「折成小写再比」的键：那与 EqualFold 并不等价，希腊字母的 σ 与词尾 ς
// 是同一个字母的大小写折叠，ToLower 却把它们折成两串，快照于是把已定价的历史用量
// 当成未定价。提供商数很少，线性扫一遍比多一条口径划算。
//
// 没填价格的条目不进快照：查不到返回 false，调用方据此计 unpriced token——「未定价」
// 与「0 元」是两件相反的事（与 Price 同口径）。
func (s *Store) PriceSnapshot() usage.PriceLookup {
	s.mu.RLock()
	defer s.mu.RUnlock()

	providers := s.viewLocked().Providers
	snap := make([]priceEntry, 0, len(providers))
	for _, p := range providers {
		models := make([]priceModel, 0, len(p.Models))
		for _, m := range p.Models {
			pm := priceModel{id: m.ID}
			if m.Price != nil {
				v := clonePrice(*m.Price)
				pm.price = &v
			}
			models = append(models, pm)
		}
		snap = append(snap, priceEntry{provider: p.Name, models: models})
	}
	return func(provider, model string) (usage.Price, bool) {
		for _, p := range snap {
			if !strings.EqualFold(p.provider, provider) {
				continue
			}
			for _, m := range p.models {
				if m.id == model && m.price != nil {
					return *m.price, true
				}
			}
			return usage.Price{}, false
		}
		return usage.Price{}, false
	}
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
	// 配置目录可能还不存在：没有 config.yaml 时它是 ~/.wen，首次运行向导是第一个写它的
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.save", "保存模型配置失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(i18n.T(lang, "modelcfg.err.save", "保存模型配置失败: %w"), err)
	}
	if err := atomicfile.Rename(tmp, s.path); err != nil {
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
func TypeOptions(lang string) []map[string]string {
	out := make([]map[string]string, 0, len(llm.KnownTypes))
	for _, t := range llm.KnownTypes {
		// 不给 default_base_url：地址由预置条目带来（见 presets.go），
		// 而「自定义」这一支预填任何一家的地址都是误导。
		out = append(out, map[string]string{
			"value": t,
			"label": llm.TypeLabel(lang, t),
		})
	}
	return out
}

// DialectOptions 返回界面下拉需要的思考参数方言候选。
func DialectOptions(lang string) []map[string]string {
	out := make([]map[string]string, 0, len(llm.KnownDialects))
	for _, d := range llm.KnownDialects {
		out = append(out, map[string]string{"value": d, "label": llm.DialectLabel(lang, d)})
	}
	return out
}

// ProtocolOptions 返回界面下拉需要的线上协议候选。
func ProtocolOptions(lang string) []map[string]string {
	out := make([]map[string]string, 0, len(llm.KnownProtocols))
	for _, p := range llm.KnownProtocols {
		out = append(out, map[string]string{"value": p, "label": llm.ProtocolLabel(lang, p)})
	}
	return out
}
