package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"wen/internal/llm"
)

// Tool 是模型可调用的工具，由插件提供。
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage // 参数 JSON Schema
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// InitContext 是核心传给插件的运行环境。
type InitContext struct {
	Workdir string // agent 工作目录
	// Lang 是进程级的兜底语言（zh / en / …），供没有 HTTP 请求可依附的路径使用：
	// 插件注入模型的提示词（走 internal/prompt）、状态行与会话注记（走 internal/i18n）。
	// 核心只负责把它交下来，不知道插件会拿它去翻什么。
	//
	// 插件应在 Init 里存下自己那份并自行加锁——Init 可能被反复调用（SetConfig 会
	// 重新 Init），此时可能有 in-flight 的 Execute 正在读。为空表示按中文处理：
	// 两个语言查表都对空语言直接回退到代码里的中文原文，不会 panic。
	Lang string
	// StateDir 是该插件专属的持久化目录（由 Manager 按插件名生成，可能不存在，需自行创建）。
	// 为空表示当前没有可用的持久化位置，需要落盘的插件应在 Init 中返回错误拒绝启用，
	// 不要退化到写进程当前目录。
	StateDir string
	// Sessions 是会话的只读窄查询（最近活跃的会话、某个会话是否还在）。
	// 为 nil 表示不可用，需要它的插件应在 Init 中返回错误拒绝启用。
	Sessions SessionQuery
	// SessionDir 是会话存储目录，只给需要读会话**正文**的插件（检索、归档）。
	// 只想知道「该落在哪个会话上」或「会话还在不在」的插件请用 Sessions：
	// 为一个布尔值换来全部对话的读写权限不划算。写入一律走 StateDir。
	SessionDir string
	// Complete 用当前模型做一次一问一答的辅助调用（不带工具、不启用思考、不写入会话），
	// 供插件完成提炼、归类这类内部工作。为 nil 表示当前不可用，插件应据此降级而不是报错。
	// 注意每次调用都产生真实的模型开销，不要放在高频路径上。
	Complete CompleteFunc
	// RunTurn 以插件身份在指定会话上跑一轮完整对话（写入会话、带工具、注入 system 提示词）。
	// Manager 交给插件前会自动注入发起方标记；会话忙时返回 ErrSessionBusy 不排队。
	// 不注入确认通道时，需要用户确认的危险操作会按拒绝处理——这是无人值守场景想要的默认。
	// 为 nil 表示当前不可用，插件应降级。每次调用都是一轮真实的模型对话开销。
	RunTurn RunTurnFunc
	// NewSession 新建一个会话并返回其 ID。为 nil 表示不可用。
	NewSession NewSessionFunc
	// Compact 压缩指定会话的历史。会话忙时返回 ErrSessionBusy。为 nil 表示不可用。
	Compact CompactFunc
	// Notice 往一个会话里追加一行只给人看的注记：落盘、在界面实时展示，但永不进入
	// 模型上下文。供后台工作交代自己做了什么——轮次收尾后事件流就关了，结果否则只能
	// 进日志。标签取自 ctx 的可见域，发起方由 Manager 注入。为 nil 表示不可用。
	Notice NoticeFunc
	// Status 取当前模型配置与（可选的）会话用量快照，供插件向远端界面展示状态，
	// 与 Web UI 的状态命令同源。为 nil 表示不可用。
	Status StatusFunc
	// DayFacts 问「某一天有什么可说的」，汇总各启用插件的回答（见 DayReporter）。
	// 日期是本地时区的 2006-01-02。为 nil 表示不可用，插件应据此降级——
	// 补不上那一句不该让功能本身停摆。
	DayFacts DayFactsFunc
}

// StatusInfo 是当前模型配置与会话用量的快照。
type StatusInfo struct {
	Version       string // 程序版本号（形如 v0.1.0），远端状态输出的首行展示用
	Provider      string
	Model         string
	Thinking      string
	ContextLength int
	// 会话部分：HasSession 为 false 时以下字段无意义（sessionID 为空或会话不存在）。
	HasSession     bool
	MessageCount   int
	MeasuredTokens int // 最近一轮实测上下文用量；<0 表示无实测值，退用 EstTokens
	EstTokens      int // 全量历史估算
	// CachedTokens 是最近一轮里命中提示词缓存的输入 token，CacheWriteTokens 是写入的。
	// 两者都为 0 表示这一轮没用上缓存（或提供商不报此项）。PromptTokens 是本轮输入
	// 总量，用于算命中比例（MeasuredTokens 含输出，不能拿来当分母）。
	CachedTokens     int
	CacheWriteTokens int
	PromptTokens     int
	// CompactAt 是自动压缩的触发点（token 数）：会话用量达到它时核心会自动压缩。
	// 0 表示未知（上下文窗口未配置）。
	CompactAt int
	// PluginLines 是启用插件贡献的状态行（见 StatusReporter），已按注册顺序排好，
	// 直接逐行输出即可。核心不解释其内容。
	PluginLines []string
}

// CompactLoad 返回会话用量占自动压缩点的比例：1 表示到了自动压缩的地步，可能略超过 1。
// 用量取实测与全量估算的较大者，与核心自动压缩的判据一致。没有会话或触发点未知时返回 0。
func (s StatusInfo) CompactLoad() float64 {
	if !s.HasSession || s.CompactAt <= 0 {
		return 0
	}
	return float64(max(s.MeasuredTokens, s.EstTokens)) / float64(s.CompactAt)
}

// StatusFunc 见 InitContext.Status。sessionID 可为空（只要模型配置部分）。
type StatusFunc func(sessionID string) (StatusInfo, error)

// CompleteFunc 见 InitContext.Complete。
type CompleteFunc func(ctx context.Context, prompt string) (string, error)

// CompactEvent 描述一次会话历史压缩。
// 历史带可见域标签时按标签分组压缩，每组产生一次独立的事件。
type CompactEvent struct {
	SessionID string
	Scope     string        // 本组历史所属的可见域标签（空串 = 共享）
	History   []llm.Message // 即将被摘要替换掉的这一组历史
	Summary   string        // 已生成的摘要正文
}

// CompactObserver 是插件的可选能力：在会话历史被压缩前收到通知。
// 未实现该接口的插件不会收到任何通知。与 TurnObserver 同一族：都是「观察核心的
// 某个时刻」，命名与广播方式保持一致。
type CompactObserver interface {
	// OnCompact 在会话历史被摘要替换之前调用，此时 History 尚未被删除。
	// 返回的非空注记会追加到摘要消息末尾；返回 error 只记录日志，不阻断压缩。
	OnCompact(ctx context.Context, ev CompactEvent) (string, error)
}

// Plugin 是系统插件的最小协议。
// 所有提示词与描述遵循项目约定：中文、只写功能、不含身份信息。
type Plugin interface {
	Name() string        // 唯一标识（小写下划线）
	Description() string // 一句话说明，供 /plugins 列表展示
	// Init 在插件启用时调用；cfg 来自配置文件 plugins.<name>.config，可能为 nil。
	Init(ictx InitContext, cfg map[string]any) error
	// SystemPrompt 返回注入系统提示词的片段；空串表示不注入。
	SystemPrompt() string
	// Tools 返回插件提供的工具；可为空。
	Tools() []Tool
}

// 可配置项的类型取值。
const (
	FieldInt    = "int"
	FieldBool   = "bool"
	FieldString = "string"
	FieldSelect = "select"
	FieldText   = "text" // 多行文本（界面上渲染成 textarea）
)

// ConfigOption 是 select 型配置项的一个候选值。
type ConfigOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	// Unavailable 标记这个候选当前用不了（如所指的通道尚未启用）。它仍然是个
	// 合法取值——单选框的取值一旦不在候选里，整份配置就保存不了，那样临时关掉
	// 一条通道会连带让这里的配置失效。
	//
	// 标记而不是让插件自己往 Label 后面缀一句「（未启用）」：那句话得跟着界面
	// 语言变，而插件是进程级单实例、不知道是谁在看。缀字的动作因此挪到翻译
	// 那一层（见 Localize），插件只说事实。
	Unavailable bool `json:"unavailable,omitempty"`
}

// ConfigField 描述插件的一个可配置项，Web UI 据此生成表单。
type ConfigField struct {
	Key         string         `json:"key"`
	Label       string         `json:"label"`
	Type        string         `json:"type"`
	Description string         `json:"description,omitempty"`
	Default     any            `json:"default"`
	Min         *int           `json:"min,omitempty"`     // 仅 int 型
	Max         *int           `json:"max,omitempty"`     // 仅 int 型
	Options     []ConfigOption `json:"options,omitempty"` // 仅 select 型
}

// Configurable 是插件的可选能力：声明可在界面上配置的项。
// 未实现该接口的插件在界面上不显示配置入口。
type Configurable interface {
	ConfigFields() []ConfigField
}

// IntPtr 便于在字段声明中书写 Min/Max。
func IntPtr(v int) *int { return &v }

// ConfigFieldsOf 返回插件声明的可配置项；未实现 Configurable 时返回 nil。
func ConfigFieldsOf(p Plugin) []ConfigField {
	if c, ok := p.(Configurable); ok {
		return c.ConfigFields()
	}
	return nil
}

// NormalizeConfig 按字段声明校验并规整配置：
// 未声明的键被忽略，缺失的键取默认值，字符串形式的数字/布尔值会被转换。
//
// 空串的处理按类型区分：数值/开关/单选的空输入表示「用默认值」（界面对清空的
// number input 提交的就是空串），而文本与多行文本的空串是合法取值——否则用户
// 清空一个文本框后保存，会看到默认值又长回来，字段永远清不掉。
func NormalizeConfig(fields []ConfigField, in map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		raw, ok := in[f.Key]
		switch {
		case !ok || raw == nil:
			raw = f.Default
		case raw == "" && f.Type != FieldString && f.Type != FieldText:
			raw = f.Default
		}
		v, err := normalizeField(f, raw)
		if err != nil {
			return nil, err
		}
		out[f.Key] = v
	}
	return out, nil
}

func normalizeField(f ConfigField, raw any) (any, error) {
	label := f.Label
	if label == "" {
		label = f.Key
	}
	switch f.Type {
	case FieldInt:
		n, ok := toInt(raw)
		if !ok {
			return nil, fmt.Errorf("配置项 %q 需要整数", label)
		}
		if f.Min != nil && n < *f.Min {
			return nil, fmt.Errorf("配置项 %q 不能小于 %d", label, *f.Min)
		}
		if f.Max != nil && n > *f.Max {
			return nil, fmt.Errorf("配置项 %q 不能大于 %d", label, *f.Max)
		}
		return n, nil
	case FieldBool:
		switch v := raw.(type) {
		case bool:
			return v, nil
		case string:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("配置项 %q 需要布尔值", label)
			}
			return b, nil
		}
		return nil, fmt.Errorf("配置项 %q 需要布尔值", label)
	case FieldString, FieldText:
		s, ok := raw.(string)
		if !ok {
			if raw == nil {
				return "", nil
			}
			return nil, fmt.Errorf("配置项 %q 需要文本", label)
		}
		if f.Type == FieldText {
			// 换行统一成 \n：多行文本会被按行切分使用（如逐行的关键词表），
			// 残留的 \r 会跟到每行末尾，让匹配莫名失败
			s = strings.ReplaceAll(s, "\r\n", "\n")
			s = strings.ReplaceAll(s, "\r", "\n")
		}
		return s, nil
	case FieldSelect:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("配置项 %q 需要文本", label)
		}
		for _, o := range f.Options {
			if o.Value == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("配置项 %q 的取值 %q 不在候选项中", label, s)
	}
	return nil, fmt.Errorf("配置项 %q 的类型 %q 不支持", label, f.Type)
}

// toInt 接受 int / float64（须为整数）/ 数字字符串。
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int(n), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

// CfgInt 从插件配置中取整数，缺失或类型不符时返回默认值。
func CfgInt(cfg map[string]any, key string, def int) int {
	if v, ok := cfg[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		}
	}
	return def
}

// CfgString 从插件配置中取字符串，缺失时返回默认值。
func CfgString(cfg map[string]any, key, def string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return def
}

// CfgBool 从插件配置中取布尔值，缺失时返回默认值。
// 注意不能用「零值即缺失」判断：false 是合法取值。
func CfgBool(cfg map[string]any, key string, def bool) bool {
	if v, ok := cfg[key]; ok {
		switch b := v.(type) {
		case bool:
			return b
		case string:
			if parsed, err := strconv.ParseBool(b); err == nil {
				return parsed
			}
		}
	}
	return def
}
