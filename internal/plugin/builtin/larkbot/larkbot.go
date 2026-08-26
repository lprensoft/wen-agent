// Package larkbot 通过飞书 / Lark 开放平台提供单聊远程会话。
//
// 飞书（中国版，open.feishu.cn）与 Lark（国际版，open.larksuite.com）是同一套 API 的
// 两个租户域：接口、事件、SDK 完全一致，但两边的应用凭证互不通用，open_id 也是两套。
// 因此本包提供一份实现、两个插件（NewFeishu / NewLark）：一个插件只能存一份凭证，
// 分开才能同时连两边，也才有各自独立的白名单与会话绑定（StateDir 按插件名分目录）。
// 两者的全部差异集中在 variant 里。
//
// 收消息走官方 SDK 的长连接（WebSocket）：它的线格式是 protobuf 私有协议，官方不公开，
// 只能用 SDK。好处是本机就能收事件，不需要公网地址。发消息则是普通 HTTP，按本项目
// 的风格手写（tenant_access_token 缓存 + im/v1/messages），SDK 只用来收。
//
// 会话绑定、命令集（/new /status /compact /help /apply /deny）、串行处理与确认代理
// 都在公共骨架 wen/internal/imbot 里。仅响应白名单内的 open_id；白名单为空时拒绝
// 所有人（陌生 open_id 记录到日志，方便抄进白名单）。仅单聊：群聊消息忽略。
package larkbot

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/plugin"
)

const (
	defConfirmTimeout = 300 // 秒

	turnTimeout = 10 * time.Minute

	// chunkLimit 是单条消息的 rune 上限。官方文本上限 150KB、卡片 30KB，
	// 真正的约束是可读性——聊天窗口里几千字已经要滚很久了。
	chunkLimit = 3000

	formatCard  = "card"
	formatPlain = "plain"
)

// variant 是飞书与 Lark 的全部差异：同一套 API，两个互不相通的租户域。
type variant struct {
	name        string // 插件名，同时决定 StateDir，两边的凭证与会话绑定因此天然隔离
	label       string // 「飞书」/「Lark」，配置项文案由它拼出
	defaultBase string // 开放平台地址
	consoleURL  string // 开发者后台地址，写进配置项说明
}

// joinCN 把片段拼成一句中文，只在拉丁文与中日韩文字相邻处补一个空格。
//
// 产品名一个是「飞书」一个是「Lark」，同一句模板套两边：不补空格 Lark 会
// 挤在汉字中间（「接入Lark机器人」），无条件补又会让飞书两侧多出空格、
// 标点后面也跟着漏一个。按相邻字符的类别决定才两边都对。
func joinCN(parts ...string) string {
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if b.Len() > 0 && needSpace(lastRune(b.String()), firstRune(part)) {
			b.WriteByte(' ')
		}
		b.WriteString(part)
	}
	return b.String()
}

// needSpace 判定两个相邻字符之间要不要空格：一边是拉丁字母或数字、另一边是
// 中日韩文字时才要。标点两侧一律不补。
func needSpace(a, b rune) bool {
	return (isLatin(a) && isCJK(b)) || (isCJK(a) && isLatin(b))
}

func isLatin(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isCJK(r rune) bool { return r >= 0x4E00 && r <= 0x9FFF }

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

func lastRune(s string) rune {
	var last rune
	for _, r := range s {
		last = r
	}
	return last
}

var (
	feishuVariant = variant{
		name: "feishu_bot", label: "飞书",
		defaultBase: lark.FeishuBaseUrl,
		consoleURL:  "https://open.feishu.cn/app",
	}
	larkVariant = variant{
		name: "lark_bot", label: "Lark",
		defaultBase: lark.LarkBaseUrl,
		consoleURL:  "https://open.larksuite.com/app",
	}
)

// Plugin 是飞书 / Lark 机器人插件。有状态：Init 可重入（先停旧长连接），字段由 mu 保护。
type Plugin struct {
	v variant

	mu sync.Mutex

	// 配置
	lang      string // 进程语言。本插件不给模型任何东西，文案一律走界面语言包
	appID     string
	appSecret string
	apiBase   string
	whitelist map[string]bool
	format    string // formatCard / formatPlain

	client *http.Client
	tokens *tokenSource
	core   *imbot.Core // 通道无关的分发、命令、会话绑定与确认代理

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// startWS 可被测试替换，避免单测真的去连开放平台。
	startWS func(ctx context.Context, appID, appSecret, base string)
}

// NewFeishu 构造飞书（中国版）插件。
func NewFeishu() *Plugin { return newVariant(feishuVariant) }

// NewLark 构造 Lark（国际版）插件。
func NewLark() *Plugin { return newVariant(larkVariant) }

func newVariant(v variant) *Plugin {
	imbot.Declare(v.name, v.label)
	p := &Plugin{v: v}
	p.startWS = p.runWS
	return p
}

func (p *Plugin) Name() string { return p.v.name }

func (p *Plugin) Category() string { return plugin.CategoryChannel }

func (p *Plugin) Description() string {
	return joinCN("接入", p.v.label, "机器人实现单聊远程会话（长连接收事件，无需公网地址），"+
		"支持 /new /status /compact /help 命令与危险操作远程确认")
}

func (p *Plugin) SystemPrompt() string { return "" }

func (p *Plugin) Tools() []plugin.Tool { return nil }

// TurnPrompt 在本通道发起的轮次里注入「像发消息那样说话」的引导（开着「像人一样
// 发消息」时）。判定与文本都在骨架里，这里只转交；别的通道、Web UI 发起的轮次返回空串。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return "", nil
	}
	return core.TurnPrompt(ctx), nil
}

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	label := p.v.label
	fields := []plugin.ConfigField{
		{
			Key: "app_id", Label: "App ID", Type: plugin.FieldString, Default: "",
			Description: joinCN("在", label, "开发者后台（"+p.v.consoleURL+"）建一个自建应用后获得，形如 cli_xxx"),
		},
		{
			Key: "app_secret", Label: "App Secret", Type: plugin.FieldString, Default: "",
			Description: joinCN("应用凭证，只保存在本机，不会上传到任何地方。注意",
				label, "与另一版的应用凭证互不通用"),
		},
		{
			Key: "whitelist", Label: "open_id 白名单", Type: plugin.FieldText, Default: "",
			Description: "每行一个 open_id（形如 ou_xxx），只响应名单内用户；留空拒绝所有人。" +
				"陌生用户首次发消息时其 open_id 会记录到运行日志，方便抄入",
		},
		{
			Key: "api_base", Label: "API 地址", Type: plugin.FieldString, Default: p.v.defaultBase,
			Description: "一般不需要改，私有化部署时填这里，长连接也会连到该地址",
		},
		{
			Key: "confirm_timeout_sec", Label: "确认等待超时（秒）", Type: plugin.FieldInt,
			Default: defConfirmTimeout, Min: plugin.IntPtr(10), Max: plugin.IntPtr(3600),
			Description: "危险操作发出确认请求后等待 /apply 或 /deny 的时长，超时按拒绝处理",
		},
		{
			Key: "format", Label: "消息格式", Type: plugin.FieldSelect, Default: formatCard,
			Options: []plugin.ConfigOption{
				{Value: formatCard, Label: "卡片（推荐，渲染格式）"},
				{Value: formatPlain, Label: "纯文本"},
			},
			Description: joinCN(label, "的普通文本消息不渲染 markdown，直发会满屏星号。"+
				"卡片：用富文本卡片发送，加粗、链接、代码等照常渲染；纯文本：发送前转成可读纯文本"),
		},
		{
			Key: "show_thinking", Label: "展示思考过程", Type: plugin.FieldBool, Default: false,
			Description: "把每轮的思考过程也推送过来（可能很长）",
		},
		{
			Key: "show_tools", Label: "展示工具调用", Type: plugin.FieldBool, Default: false,
			Description: "把每轮调用了哪些工具也推送过来（只有工具名，不含参数与结果）",
		},
		{
			Key: "push_notices", Label: "推送后台通知", Type: plugin.FieldBool, Default: false,
			Description: "把后台工作留下的说明也推送过来（如记忆提炼的记录）。只推共享内容，推送本身不写入会话",
		},
	}
	// 收发节奏的两项与别的通道共用一份声明，见 imbot.PaceFields
	return append(fields, imbot.PaceFields()...)
}

// Init 应用配置并（重）启动长连接。可重入：先停旧连接与骨架。
// langOf 取进程语言。
func (p *Plugin) langOf() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	appID := strings.TrimSpace(plugin.CfgString(cfg, "app_id", ""))
	appSecret := strings.TrimSpace(plugin.CfgString(cfg, "app_secret", ""))
	if appID == "" || appSecret == "" {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.lark_bot.err.no_credentials",
			"请先在配置中填写 App ID 与 App Secret（%s）"),
			joinCN(p.v.label, i18n.Tf(ictx.Lang, "plugin.lark_bot.err.console",
				"开发者后台：%s", p.v.consoleURL)))
	}
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.lark_bot.err.no_state_dir",
			"没有可用的持久化目录"))
	}
	if err := os.MkdirAll(ictx.StateDir, 0o755); err != nil {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.lark_bot.err.mkdir",
			"创建状态目录失败: %w"), err)
	}

	whitelist := map[string]bool{}
	for _, line := range strings.Split(plugin.CfgString(cfg, "whitelist", ""), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			whitelist[s] = true
		}
	}
	base := strings.TrimRight(plugin.CfgString(cfg, "api_base", p.v.defaultBase), "/")

	mergeWindow, humanPace := imbot.PaceConfig(cfg)

	p.Stop()

	p.mu.Lock()
	p.lang = ictx.Lang
	p.appID, p.appSecret = appID, appSecret
	p.apiBase = base
	p.whitelist = whitelist
	p.format = plugin.CfgString(cfg, "format", formatCard)
	p.client = &http.Client{Timeout: apiTimeout}
	p.tokens = newTokenSource(appID, appSecret, base, p.client)
	p.mu.Unlock()

	core, err := imbot.New(imbot.Config{
		PluginName:     p.Name(),
		Sender:         sender{p: p},
		Lang:           ictx.Lang,
		StateDir:       ictx.StateDir,
		ConfirmTimeout: time.Duration(plugin.CfgInt(cfg, "confirm_timeout_sec", defConfirmTimeout)) * time.Second,
		TurnTimeout:    turnTimeout,
		ShowThinking:   plugin.CfgBool(cfg, "show_thinking", false),
		ShowTools:      plugin.CfgBool(cfg, "show_tools", false),
		PushNotices:    plugin.CfgBool(cfg, "push_notices", false),
		MergeWindow:    mergeWindow,
		HumanPace:      humanPace,
		Allow:          p.allowed,
		Push:           p.push,
		Notice:         ictx.Notice,
		RunTurn:        ictx.RunTurn,
		NewSession:     ictx.NewSession,
		Compact:        ictx.Compact,
		Status:         ictx.Status,
		Sessions:       ictx.Sessions,
	})
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.core = core
	p.ctx, p.cancel = context.WithCancel(context.Background())
	ctx := p.ctx
	start := p.startWS
	p.mu.Unlock()

	core.Start(ctx)
	start(ctx, appID, appSecret, base)
	return nil
}

// allowed 判定用户是否放行：只看配置白名单，空名单拒绝所有人。
func (p *Plugin) allowed(openID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return openID != "" && p.whitelist[openID]
}

// Stop 断开长连接并停掉骨架的全部 worker。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel, core := p.cancel, p.core
	p.cancel = nil
	p.mu.Unlock()
	if core != nil {
		core.Stop()
	}
	if cancel != nil {
		cancel()
		p.wg.Wait()
	}
}

// sender 把骨架的发送请求接到飞书 / Lark 的协议层。
type sender struct{ p *Plugin }

func (s sender) Send(ctx context.Context, userID, text, replyTo string) {
	s.p.send(ctx, userID, text, replyTo)
}
