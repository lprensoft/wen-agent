package plugin

import (
	"context"
	"strings"
)

// ActionDef 描述插件对外暴露的一个操作入口（设置页插件卡片上的按钮）。
// Label 可随插件状态变化（如「扫码绑定」在绑定后变「重新绑定」），界面每次列表刷新取最新值。
type ActionDef struct {
	Key         string `json:"key"` // 限小写字母开头的小写字母、数字与下划线
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Detail 是这个操作当前的**运行时事实**：技能目录在哪儿、已经装了哪几个插件包、
	// 有没有新版本可用。展示时接在 Description 后面。
	//
	// 与 Description 分开，是因为 Description 要能进语言包，而掺了运行时内容的句子
	// 进不去——键查出来的是一句固定的话，那个路径、那个版本号会在翻译时凭空消失。
	// 所以插件只说事实，缀字的动作挪到翻译那一层（见 Localize），与 ConfigOption
	// 的 Unavailable 是同一条道理。Detail 本身不翻译：它是路径与名字。
	Detail string `json:"detail,omitempty"`
	// Token 是这个按钮此刻**指的是哪一个东西**：插件列出操作时填上，界面点按钮时原样
	// 带回来（经 ctx，见 ActionTokenFrom）。核心与界面都不解释它。
	//
	// 存在的理由是「按钮的含义取决于展示时的内容」：批准的是说明里显示的那一条提议、
	// 安装的是显示的那个版本。按下时让插件重新挑一次「当前那个」是不够的——两个窗口
	// 各开一份设置页，一边处理掉了 A、刷新出 B，另一边停在 A 上的按钮按下去就成了 B。
	// 插件也不能自己记「最后一次展示的是哪个」：那是全局的，任何一个客户端的刷新都会
	// 改掉它。只有请求本身带着它看到的东西，才答得了「这个人点的是哪一个」。
	Token string `json:"token,omitempty"`
}

// actionTokenKey 是 ctx 中展示凭据的键。
type actionTokenKey struct{}

// WithActionToken 把按钮展示时的凭据（ActionDef.Token）附到 ctx 上，供操作核对。
// 空串不附。
func WithActionToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, actionTokenKey{}, token)
}

// ActionTokenFrom 取出本次操作附带的展示凭据；没有则返回空串。与 ActionValuesFrom
// 一样必须在 StartAction 内同步取出。值来自客户端，插件要把它当作不可信的输入核对。
func ActionTokenFrom(ctx context.Context) string {
	v, _ := ctx.Value(actionTokenKey{}).(string)
	return v
}

// 操作的运行状态。
const (
	ActionIdle    = "idle"    // 尚未开始（或上次结果已被新一次覆盖前的初始态）
	ActionPending = "pending" // 进行中，界面应继续轮询
	ActionDone    = "done"    // 成功结束
	ActionError   = "error"   // 失败结束
)

// ActionState 是操作的当前进展，供界面轮询展示。
// 核心不关心 Image 与 Link 的内容——它们只是「操作过程中需要给用户看的东西」，
// 只经内存下发，不落盘。
type ActionState struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"` // 人类可读的进展或结果说明
	// Markdown 声明 Message 是 markdown，界面按 markdown 渲染它。
	//
	// 由产出方声明而不是由界面去猜：绝大多数操作给的是纯文本（一行结论、一份清单），
	// 拿 markdown 去解析它们，正文里的 * 或 _ 就会变成强调、四个空格会变成代码块。
	// 只有自己知道「我这段正文来自别处的 markdown」的插件才该打开它——程序更新那段
	// 更新说明取自 GitHub 的发布正文，是现成的 markdown。
	//
	// 零值是纯文本，既有实现不受影响。
	Markdown bool   `json:"markdown,omitempty"`
	Image    []byte `json:"image,omitempty"` // 可选 PNG，界面渲染为 data URI
	// Link 是操作涉及的一个链接原文（如编码进二维码的 URL）。图形界面用 Image，
	// 终端界面拿它自行渲染（如字符二维码）——同一内容的两种呈现。
	Link string `json:"link,omitempty"`
}

// Actionable 由需要「用户在界面上主动触发一段流程」的插件实现（可选）。
// Actions 必须廉价（每次刷新插件列表都会调用，与 SystemPrompt 同一契约）；
// StartAction 应立即返回，长流程放后台 goroutine 并自带超时；进行中重复调用表示重新开始。
// 实现自行对状态加锁——StartAction 与 ActionState 会被 HTTP 处理并发调用。
// ctx 可能带有配置弹窗里尚未保存的草稿值，用 ActionValuesFrom 取（须在 StartAction 内同步取出）。
type Actionable interface {
	Actions() []ActionDef
	StartAction(ctx context.Context, key string) error
	ActionState(key string) (ActionState, error)
}

// ---------- 操作的草稿配置值 ----------

// actionValuesKey 是 ctx 中草稿配置值的键。
type actionValuesKey struct{}

// WithActionValues 把「配置弹窗里当前填写、尚未保存的值」附到 ctx 上，供操作读取。
//
// 存在的理由是「测试」类操作：用户填完一项配置想先验证它能不能用，再决定要不要保存，
// 而 StartAction 只能看到已保存的配置。走 ctx 而不是给 StartAction 加参数，是为了不动
// 已有实现的签名——不关心草稿值的操作照旧不用管它。
//
// 值未经校验，就是界面原样提交的表单内容，插件自行按 CfgString / CfgInt 取用。
func WithActionValues(ctx context.Context, values map[string]any) context.Context {
	if len(values) == 0 {
		return ctx
	}
	return context.WithValue(ctx, actionValuesKey{}, values)
}

// ActionValuesFrom 取出本次操作附带的草稿配置值；没有则返回 nil。
//
// 必须在 StartAction 内**同步**取出：ctx 属于那个 HTTP 请求，响应发出后就失效了，
// 而长流程是在后台 goroutine 里跑的，拿着 ctx 过去读会读到已经作废的东西。
func ActionValuesFrom(ctx context.Context) map[string]any {
	v, _ := ctx.Value(actionValuesKey{}).(map[string]any)
	return v
}

// ActionValueOr 从草稿值里取一个字符串配置项，草稿里没有（或为空）时回落到 saved。
// 「测试」类操作取参数的通用写法：以界面上正在填的为准，没填过就用已保存的。
func ActionValueOr(values map[string]any, key, saved string) string {
	if v := strings.TrimSpace(CfgString(values, key, "")); v != "" {
		return v
	}
	return saved
}
