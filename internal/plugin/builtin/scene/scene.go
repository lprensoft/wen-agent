package scene

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	defaultMaxScenes      = 30
	defaultMaxInjectBytes = 8 * 1024
	// maxStageBytes 限制舞台设定的长度：它每轮随 system 消息全额重发且不参与预算裁剪，
	// 必须有硬上限。超出时截断并注明，而不是整段丢弃。
	maxStageBytes = 8 * 1024
)

// Plugin 是 scene 系统插件。
type Plugin struct {
	mu sync.RWMutex

	lang           string
	stage          string // 用户配置的场景与环境设定（已按上限截断）
	base           string // 基准场景库目录（= 共享域）
	maxScenes      int
	maxInjectBytes int

	// 按可见域分出的库，惰性创建。单独一把锁：这张表的生命周期与配置无关。
	storesMu sync.Mutex
	stores   map[string]*Store
}

func New() *Plugin {
	return &Plugin{
		maxScenes:      defaultMaxScenes,
		maxInjectBytes: defaultMaxInjectBytes,
	}
}

func (p *Plugin) Name() string { return "scene" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }
func (p *Plugin) Description() string {
	return "场景感知：注入场景与环境设定作为演绎舞台，并把对话中出现的场景与地点记成场景记忆"
}

// Requires 硬依赖 roleplay：舞台是为角色扮演搭的，没有角色，场景设定与场景记忆
// 都没有作用对象。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "stage", Label: "场景与环境设定", Type: plugin.FieldText,
			Description: "对话发生的场景或环境（地点、时代、氛围等），作为演绎的舞台背景每轮注入。" +
				"留空则不注入固定舞台，只做场景记录。",
			Default: "",
		},
		{
			Key: "max_scenes", Label: "注入的场景条数上限", Type: plugin.FieldInt,
			Description: "超出后按最近更新保留，其余模型仍可自行查看，不会丢失。",
			Default:     defaultMaxScenes,
			Min:         plugin.IntPtr(0),
			Max:         plugin.IntPtr(500),
		},
		{
			Key: "max_inject_bytes", Label: "场景记忆注入字节上限", Type: plugin.FieldInt,
			Description: "场景记忆随每轮对话重复发送，因此需要上限。" +
				"超出时先省略描述只留名称，仍超出则只注明条数。",
			Default: defaultMaxInjectBytes,
			Min:     plugin.IntPtr(512),
			Max:     plugin.IntPtr(64 * 1024),
		},
	}
}

// Init 需要一个可写目录；没有则拒绝启用，避免把场景记忆散落到进程当前目录。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("没有可用的持久化目录，无法保存场景记忆")
	}

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.stage = clip(ictx.Lang, strings.TrimSpace(plugin.CfgString(cfg, "stage", "")), maxStageBytes)
	p.base = filepath.Join(ictx.StateDir, "scenes")
	p.maxScenes = plugin.CfgInt(cfg, "max_scenes", defaultMaxScenes)
	p.maxInjectBytes = plugin.CfgInt(cfg, "max_inject_bytes", defaultMaxInjectBytes)
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{
		&saveTool{p: p},
		&listTool{p: p},
		&deleteTool{p: p},
	}
}

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang           string
	stage          string
	base           string
	maxScenes      int
	maxInjectBytes int
}

// snapshot 取一份配置快照：SetConfig 会在运行时重新 Init，而工具可能正在执行。
func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang:           p.lang,
		stage:          p.stage,
		base:           p.base,
		maxScenes:      p.maxScenes,
		maxInjectBytes: p.maxInjectBytes,
	}
}

const stageHeaderZH = `[场景与环境]
以下是对话发生的场景与环境设定，作为演绎的舞台背景。位置、环境与氛围的描写以此为
基准，不与之矛盾；设定之外的细节可以合理补充，但一旦写出就保持前后一致。`

const guidePromptZH = `[场景感知]
对话中凡是出现场景或地理位置的描述——所处的城市、街区、建筑、房间、店铺，或反复
出现的活动场所——都用 save_scene 记录下来，使场景在之后的对话中得以延续。
- 记录前先查看 [场景记忆] 中是否已有相同或同一地点的条目：已有且信息一致就不再
  记录；信息有补充或变化时用 replace 模式更新原条目，不另建新条目。
- 只记场景本身：位置、与其它地点的关系、布局、陈设、氛围与显著细节，不记一次性的
  动作与对话内容。
- 场景已不复存在或彻底改变时，用 replace 更新或用 delete_scene 移除。`

const scenesHeaderZH = `[场景记忆]
以下是此前记录下来的场景与地点。演绎到相关位置时保持与这些描述一致，已有的条目
不重复记录：`

// SystemPrompt 注入舞台设定与场景记录的判据，均为静态内容。
// 场景记忆改由 TurnPrompt 注入：它取决于本轮的可见域，且要访问磁盘，
// 而本方法的契约是廉价、无副作用（列表接口会对禁用的插件也调用它）。
func (p *Plugin) SystemPrompt() string {
	s := p.snapshot()
	guide := prompt.T(s.lang, "prompt.scene.guide", guidePromptZH)
	if s.stage == "" {
		return guide
	}
	return prompt.T(s.lang, "prompt.scene.stage_header", stageHeaderZH) + "\n\n" + s.stage + "\n\n" + guide
}

// TurnPrompt 注入本轮可读的场景记忆。库为空时不注入：记录判据已在 SystemPrompt 里。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	s := p.snapshot()
	if s.base == "" {
		return "", nil
	}
	scenes, err := p.visibleScenes(ctx)
	if err != nil || len(scenes) == 0 {
		return "", err
	}
	return prompt.T(s.lang, "prompt.scene.scenes_header", scenesHeaderZH) + "\n" +
		renderScenes(s.lang, scenes, s.maxScenes, s.maxInjectBytes), nil
}

// ---------- 按可见域分库 ----------

// storeFor 返回某个可见域的场景库（惰性创建）。未初始化时返回 nil。
func (p *Plugin) storeFor(tag string) *Store {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return nil
	}

	p.storesMu.Lock()
	defer p.storesMu.Unlock()
	if p.stores == nil {
		p.stores = map[string]*Store{}
	}
	dir := plugin.DomainDir(base, tag)
	if s, ok := p.stores[tag]; ok {
		return s
	}
	s := NewStore(p.snapshot().lang, dir)
	p.stores[tag] = s
	return s
}

// writeStore 返回本轮该写入的库。
func (p *Plugin) writeStore(ctx context.Context) *Store {
	return p.storeFor(plugin.ScopeFrom(ctx).Write)
}

// visibleScenes 合并本轮所有可读域的场景，保持各库内的记录顺序，写入域在前。
// 同名场景只保留一条，取自靠前的域：模型看到的是一份没有重影的清单，
// delete 也就落在同一条上。
func (p *Plugin) visibleScenes(ctx context.Context) ([]Scene, error) {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()

	var (
		out  []Scene
		seen = map[string]bool{}
		errs []string
	)
	for _, tag := range plugin.ReadDomains(base, plugin.ScopeFrom(ctx)) {
		s := p.storeFor(tag)
		if s == nil {
			continue
		}
		scenes, err := s.List()
		if err != nil {
			errs = append(errs, err.Error())
			continue // 单个域读不出来不该让其余域也用不了
		}
		for _, sc := range scenes {
			key := strings.ToLower(sc.Name)
			if seen[key] {
				continue
			}
			seen[key] = true
			sc.Domain = tag
			out = append(out, sc)
		}
	}
	if out == nil && len(errs) > 0 {
		return nil, fmt.Errorf(prompt.T(p.snapshot().lang, "prompt.scene.result.read_failed",
			"读取场景记忆失败: %s"), strings.Join(errs, "; "))
	}
	return out, nil
}

// findVisible 在本轮可读的场景里按名称查找，并给出它所属的库。
func (p *Plugin) findVisible(ctx context.Context, name string) (Scene, *Store, error) {
	scenes, err := p.visibleScenes(ctx)
	if err != nil {
		return Scene{}, nil, err
	}
	if i := indexOf(scenes, name); i >= 0 {
		return scenes[i], p.storeFor(scenes[i].Domain), nil
	}
	return Scene{}, nil, fmt.Errorf(prompt.T(p.snapshot().lang, "prompt.scene.result.not_found",
		"没有名为 %q 的场景"), strings.TrimSpace(name))
}

// ---------- 注入渲染 ----------

// renderScenes 按预算渲染场景清单，分三级降级：
// 全列（名称 + 描述）→ 省略描述只留名称 → 只注明条数。
// 降级优先砍描述而不是砍条目：名称在，模型才知道这处场景存在，不会重复记录。
func renderScenes(lang string, scenes []Scene, maxScenes, maxBytes int) string {
	kept, dropped := limitScenes(scenes, maxScenes)

	if out, ok := renderLines(lang, kept, dropped, maxBytes, true); ok {
		return out
	}
	if out, ok := renderLines(lang, kept, dropped, maxBytes, false); ok {
		return out
	}
	// 仍然超预算：按最近更新丢弃更多条目，直到装得下
	for n := len(kept) - 1; n > 0; n-- {
		trimmed, more := limitScenes(scenes, n)
		if out, ok := renderLines(lang, trimmed, more, maxBytes, false); ok {
			return out
		}
	}
	return prompt.Tf(lang, "prompt.scene.too_many",
		"（共 %d 处场景，注入预算不足以列出，可用 list_scenes 查看）", len(scenes))
}

// limitScenes 在超出条数上限时按最近更新保留，返回保留的条目（仍是原顺序）与被略去的条数。
func limitScenes(scenes []Scene, max int) ([]Scene, int) {
	if max <= 0 || len(scenes) <= max {
		return scenes, 0
	}
	recent := make([]Scene, len(scenes))
	copy(recent, scenes)
	// 只用更新时间挑选保留哪些，不用它决定展示顺序：记录顺序承载着舞台展开的脉络
	slices.SortStableFunc(recent, func(a, b Scene) int {
		return b.Updated.Compare(a.Updated)
	})
	keep := map[string]bool{}
	for _, sc := range recent[:max] {
		keep[strings.ToLower(sc.Name)] = true
	}
	out := make([]Scene, 0, max)
	for _, sc := range scenes {
		if keep[strings.ToLower(sc.Name)] {
			out = append(out, sc)
		}
	}
	return out, len(scenes) - len(out)
}

func renderLines(lang string, scenes []Scene, dropped, maxBytes int, withDetail bool) (string, bool) {
	var b strings.Builder
	for _, sc := range scenes {
		b.WriteString("- ")
		b.WriteString(sc.Name)
		if withDetail {
			b.WriteString("：")
			b.WriteString(sc.Detail)
		}
		b.WriteString("\n")
	}
	if dropped > 0 {
		fmt.Fprintf(&b, prompt.T(lang, "prompt.scene.more_hidden",
			"（另有 %d 处未列出，可用 list_scenes 查看）\n"), dropped)
	}
	out := strings.TrimRight(b.String(), "\n")
	if maxBytes > 0 && len(out) > maxBytes {
		return "", false
	}
	return out, true
}

// clip 按字节上限截断，不切断 UTF-8 字符。超出时截断并注明，而不是整段丢弃。
func clip(lang, s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	note := prompt.T(lang, "prompt.scene.clipped", "…（设定过长已截断）")
	cut := limit - len(note)
	if cut <= 0 {
		return ""
	}
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	if cut == 0 {
		return ""
	}
	return s[:cut] + note
}
