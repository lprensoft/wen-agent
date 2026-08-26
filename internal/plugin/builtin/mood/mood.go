package mood

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	defaultDecayPerHour = 5
	defaultMaxDelta     = 30
)

// Plugin 是 mood 系统插件。
type Plugin struct {
	mu sync.RWMutex

	lang         string // 进程语言，注入片段与工具文本据此取用
	base         string // 基准心情库目录（= 共享域）
	decayPerHour int
	maxDelta     int

	// 按可见域分出的库，惰性创建。单独一把锁：这张表的生命周期与配置无关。
	storesMu sync.Mutex
	stores   map[string]*Store

	// 设置页操作的状态。StartAction 与 ActionState 会被 HTTP 并发调用，自带一把锁。
	actMu    sync.Mutex
	actState plugin.ActionState
}

func New() *Plugin {
	return &Plugin{decayPerHour: defaultDecayPerHour, maxDelta: defaultMaxDelta}
}

func (p *Plugin) Name() string { return "mood" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "心情：角色此刻的心情由互动的演绎增减，随时间向平静回落，每轮注入并影响表达方式"
}

// Requires 硬依赖 roleplay：没有角色，就没有「谁的心情」。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "decay_per_hour", Label: "每小时回落点数", Type: plugin.FieldInt,
			Description: "心情每过一小时向平静靠拢多少点。真人不会把一次不快原样带到三天以后；" +
				"填 0 表示不回落，心情会一直停在上次的值。",
			Default: defaultDecayPerHour,
			Min:     plugin.IntPtr(0),
			Max:     plugin.IntPtr(100),
		},
		{
			Key: "max_delta_per_call", Label: "单次调整幅度上限", Type: plugin.FieldInt,
			Description: "一次调整最多改变多少点，决定情绪起伏的剧烈程度。" +
				"调小则心情变化平缓，调大则一句话就能从开心到崩溃。超出的部分按上限收，并告知模型。",
			Default: defaultMaxDelta,
			Min:     plugin.IntPtr(1),
			Max:     plugin.IntPtr(moodMax - moodMin),
		},
	}
}

// Init 需要一个可写目录；没有则拒绝启用，避免把心情散落到进程当前目录。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.mood.no_state_dir",
			"没有可用的持久化目录，无法保存心情"))
	}

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.base = filepath.Join(ictx.StateDir, "mood")
	p.decayPerHour = plugin.CfgInt(cfg, "decay_per_hour", defaultDecayPerHour)
	p.maxDelta = plugin.CfgInt(cfg, "max_delta_per_call", defaultMaxDelta)
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&adjustTool{p: p}}
}

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang         string
	base         string
	decayPerHour int
	maxDelta     int
}

// snapshot 取一份配置快照：SetConfig 会在运行时重新 Init，而工具可能正在执行。
func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{lang: p.lang, base: p.base, decayPerHour: p.decayPerHour, maxDelta: p.maxDelta}
}

// SystemPrompt 注入调整判据与心情的作用，均为静态内容。
// 当前心情改由 TurnPrompt 注入：它取决于本轮的可见域，且要访问磁盘，
// 而本方法的契约是廉价、无副作用（列表接口会对禁用的插件也调用它）。
func (p *Plugin) SystemPrompt() string {
	lang := p.langOf()
	return guideFor(lang) + "\n\n" + effectFor(lang)
}

// langOf 取进程语言。SystemPrompt 与 storeFor 在快照之外单独取一次，
// 它们不需要整份配置；零值（未初始化的插件）表示回落中文。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// TurnPrompt 注入本轮的当前心情。没有过记录时不注入：提示词里已经写了按平静起算。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	s := p.snapshot()
	store := p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", nil
	}
	st, ok, err := store.Current(s.decayPerHour, time.Now())
	if err != nil || !ok {
		return "", err
	}
	return stateHeaderFor(s.lang) + "\n" + renderState(s.lang, st), nil
}

// ---------- 按可见域分库 ----------

// storeFor 返回某个可见域的心情库（惰性创建）。未初始化时返回 nil。
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
	if s, ok := p.stores[tag]; ok {
		return s
	}
	s := NewStore(p.langOf(), plugin.DomainDir(base, tag))
	p.stores[tag] = s
	return s
}

// 心情**只读写本轮的写入域**，不像 scene 或 body_sense 那样跨域合并。
//
// 心情是单值状态，跨域合不出有意义的结果——两个人格各有各的心情，求和、取最新、
// 回落到共享域都说不通。各域独立还顺带让泄漏问题不存在：读的就是自己那一份。
// 代价是切换人格时心情跟着换一套，而这正是「表里两套人格」该有的样子。

// ---------- 注入渲染 ----------

// band 按数值给出心情词。分档取绝对值 15 / 40 / 70 三道线，两侧对称。
func band(lang string, v int) string {
	switch {
	case v <= -70:
		return prompt.T(lang, "prompt.mood.band.despondent", "消沉")
	case v <= -40:
		return prompt.T(lang, "prompt.mood.band.sad", "难过")
	case v <= -15:
		return prompt.T(lang, "prompt.mood.band.down", "低落")
	case v < 15:
		return prompt.T(lang, "prompt.mood.band.calm", "平静")
	case v < 40:
		return prompt.T(lang, "prompt.mood.band.pleased", "愉快")
	case v < 70:
		return prompt.T(lang, "prompt.mood.band.happy", "开心")
	default:
		return prompt.T(lang, "prompt.mood.band.elated", "雀跃")
	}
}

// renderState 渲染一行当前心情。体量固定，不需要预算与降级。
//
// 不注入「距上次多久」：回落速率已经把时间编码进数值本身，再报一次间隔是重复的。
func renderState(lang string, st State) string {
	if st.Value == 0 {
		return prompt.Tf(lang, "prompt.mood.render.now_calm", "此刻：%s。",
			prompt.T(lang, "prompt.mood.band.calm", "平静"))
	}
	out := prompt.Tf(lang, "prompt.mood.render.now", "此刻：%s（%+d）。", band(lang, st.Value), st.Value)
	if st.Reason != "" {
		out += prompt.Tf(lang, "prompt.mood.render.cause", "起因：%s。", st.Reason)
	}
	return out
}
