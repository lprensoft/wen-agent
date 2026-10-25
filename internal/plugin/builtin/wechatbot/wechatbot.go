// Package wechatbot 通过微信官方 ClawBot 插件通道（iLink Bot API）提供个人微信私聊远程会话。
//
// iLink 是腾讯 2026 年正式开放的个人微信 Bot 协议：HTTP/JSON，扫码绑定后以
// bot_token 鉴权，getupdates 长轮询收消息、sendmessage 回消息（必须回带入站
// 消息的 context_token）。凭证不是可粘贴的 Key，而是动态二维码扫码确认后由
// 服务端下发，因此绑定流程通过「插件操作入口」（plugin.Actionable）做在设置页。
//
// 每个微信用户（xxx@im.wechat）映射到一个当前会话，远程会话就是普通会话，
// Web UI 刷新即可看到。会话绑定、命令集（/new /status /compact /help /apply /deny）、
// 串行处理与确认代理都在公共骨架 wen/internal/imbot 里，这个包只负责微信的协议层：
// 扫码绑定、长轮询、发送与「正在输入」。
//
// 仅响应白名单内的用户；扫码绑定人自动放行，白名单为空时其他人一律拒绝（陌生用户
// 记录到日志，方便抄进白名单）。仅私聊：带 group_id 的消息忽略（群聊能力官方未充分验证）。
package wechatbot

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"

	"wen/internal/imbot"
	"wen/internal/plugin"
)

const (
	defAPIBase        = "https://ilinkai.weixin.qq.com"
	defConfirmTimeout = 300 // 秒

	turnTimeout = 10 * time.Minute

	// chunkLimit 是单条消息的 rune 上限。官方未公开限制，保守取值，实测后可调。
	chunkLimit = 2000

	// expiredPause 是服务端返回会话过期（errcode -14）后的暂停时长，与官方参考实现一致。
	expiredPause = time.Hour
)

// 消息格式。协议层的 text_item 只有纯文本字段，但微信的 ClawBot 聊天界面会
// 渲染 Bot 文本里的 markdown（加粗、引用、代码块等），因此默认直发 markdown；
// 遇到不渲染的客户端版本时可切回纯文本（转换降级见 wen/internal/mdtext）。
const (
	formatMarkdown = "markdown"
	formatPlain    = "plain"
)

// Plugin 是微信 ClawBot 插件。有状态：Init 可重入（先停旧循环），字段由 mu 保护。
type Plugin struct {
	mu sync.Mutex

	// 配置
	lang      string // 进程语言。本插件不给模型任何东西，文案一律走界面语言包
	apiBase   string // 扫码入口用的公共基址；绑定后改用服务端下发的专属 baseurl
	whitelist map[string]bool
	format    string // 消息格式：markdown 直发 / plain 转纯文本

	// 凭证（扫码绑定后持久化到 StateDir/credentials.json）
	creds    credentials
	stateDir string

	// 运行组件
	core   *imbot.Core       // 通道无关的分发、命令、会话绑定与确认代理
	tokens *tokenStore       // 微信用户 ID → 最近入站的 context_token（后台推送用）
	typing map[string]string // 微信用户 ID → typing_ticket 缓存

	// 长轮询循环有独立的子取消：绑定成功后可只重启轮询而不动整个插件
	ctx        context.Context
	cancel     context.CancelFunc
	pollCancel context.CancelFunc
	wg         sync.WaitGroup

	// 绑定流程状态（插件操作入口）
	bind bindState

	// pausedUntil 是 -14 之后全部请求暂停到几点：收信循环自己睡，发送与输入状态按它跳过
	pausedUntil time.Time
	// 测试注入：过期暂停时长（默认 expiredPause）
	pauseOnExpired time.Duration
}

// inbound 是一条收到的私聊消息。
type inbound struct {
	userID       string // from_user_id（xxx@im.wechat）
	contextToken string // 回复时必须回带
	text         string
}

func New() *Plugin {
	imbot.Declare("wechat_bot", "微信")
	return &Plugin{}
}

func (p *Plugin) Name() string { return "wechat_bot" }

func (p *Plugin) Category() string { return plugin.CategoryChannel }

func (p *Plugin) Description() string {
	return "接入微信官方 ClawBot 插件实现个人微信私聊远程会话（设置页扫码绑定），支持 /new /status /compact /help 命令与危险操作远程确认"
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
	fields := []plugin.ConfigField{
		{
			Key: "whitelist", Label: "用户白名单", Type: plugin.FieldText, Default: "",
			Description: "每行一个微信用户 ID（形如 xxx@im.wechat）。扫码绑定人始终放行；名单外用户一律拒绝，其 ID 会记录到运行日志方便抄入",
		},
		{
			Key: "api_base", Label: "API 地址", Type: plugin.FieldString, Default: defAPIBase,
			Description: "扫码绑定的入口地址，一般不需要改；绑定后的请求走服务端下发的专属地址",
		},
		{
			Key: "confirm_timeout_sec", Label: "确认等待超时（秒）", Type: plugin.FieldInt,
			Default: defConfirmTimeout, Min: plugin.IntPtr(10), Max: plugin.IntPtr(3600),
			Description: "危险操作发出确认请求后等待 /apply 或 /deny 的时长，超时按拒绝处理",
		},
		{
			Key: "format", Label: "消息格式", Type: plugin.FieldSelect, Default: formatMarkdown,
			Options: []plugin.ConfigOption{
				{Value: formatMarkdown, Label: "markdown（推荐）"},
				{Value: formatPlain, Label: "纯文本"},
			},
			Description: "markdown：原样发送，由微信的 ClawBot 界面渲染加粗、引用等格式；" +
				"纯文本：发送前去掉格式标记，适合不显示格式的客户端",
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
	// 收发节奏的几项与别的通道共用一份声明，见 imbot.PaceFields
	return append(fields, imbot.PaceFields()...)
}

// Init 应用配置并（重）启动。可重入：先停旧循环与骨架。
// 未绑定时 Init 也成功——插件进入待绑定状态，等设置页扫码；绑定过则直接起轮询。
// langOf 取进程语言。
func (p *Plugin) langOf() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("没有可用的持久化目录")
	}
	// 目录按约定可能还不存在。收信游标是第一个落盘的东西（比会话绑定还早），
	// 不先建出来的话首条消息的游标就存不下
	if err := os.MkdirAll(ictx.StateDir, 0o755); err != nil {
		return fmt.Errorf("创建状态目录失败: %w", err)
	}
	tokens, err := loadTokens(ictx.StateDir)
	if err != nil {
		return fmt.Errorf("加载推送票据失败: %w", err)
	}
	creds, err := loadCredentials(ictx.StateDir)
	if err != nil {
		return fmt.Errorf("加载绑定凭证失败: %w", err)
	}

	whitelist := map[string]bool{}
	for _, line := range strings.Split(plugin.CfgString(cfg, "whitelist", ""), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			whitelist[s] = true
		}
	}

	mergeWindow, humanPace := imbot.PaceConfig(cfg)
	holdWhenBusy, wakeAfter := imbot.HoldConfig(cfg)

	p.Stop()

	p.mu.Lock()
	p.lang = ictx.Lang
	p.apiBase = strings.TrimRight(plugin.CfgString(cfg, "api_base", defAPIBase), "/")
	p.whitelist = whitelist
	p.format = plugin.CfgString(cfg, "format", formatMarkdown)
	p.creds = creds
	p.pausedUntil = time.Time{} // 重新 Init（含换凭证）后旧暂停不再作数；收信循环遇到 -14 会重新设
	p.stateDir = ictx.StateDir
	p.tokens = tokens
	p.typing = map[string]string{}
	if p.pauseOnExpired == 0 {
		p.pauseOnExpired = expiredPause
	}
	coreCfg := imbot.Config{
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
		HoldWhenBusy:   holdWhenBusy,
		WakeAfter:      wakeAfter,
		Allow:          p.allowed,
		Push:           p.push,
		Notice:         ictx.Notice,
		OnAccepted:     p.rememberToken,
		Typing:         p.onTyping,
		RunTurn:        ictx.RunTurn,
		NewSession:     ictx.NewSession,
		Compact:        ictx.Compact,
		Status:         ictx.Status,
		Sessions:       ictx.Sessions,
	}
	p.mu.Unlock()

	core, err := imbot.New(coreCfg)
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.core = core
	p.ctx, p.cancel = context.WithCancel(context.Background())
	ctx := p.ctx
	bound := creds.valid()
	p.mu.Unlock()

	core.Start(ctx)
	if bound {
		p.startPolling()
	} else {
		i18n.Logf(p.langOf(), "log.wechat.not_bound_hint", "wechat_bot: 尚未绑定微信，请到设置页的插件卡片上点击「扫码绑定微信」")
	}
	return nil
}

// allowed 判定用户是否放行：扫码绑定人始终放行，其余看配置白名单。
func (p *Plugin) allowed(userID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return userID != "" && (userID == p.creds.BinderUserID || p.whitelist[userID])
}

// rememberToken 记住该用户最近一次入站的 context_token：后台轮次（心跳等）的结果要靠它推送。
func (p *Plugin) rememberToken(msg imbot.Message) {
	p.mu.Lock()
	tokens := p.tokens
	p.mu.Unlock()
	tokens.remember(p.langOf(), msg.UserID, msg.ReplyTo)
}

// onTyping 把骨架的「正在输入」回调接到微信协议层。
//
// 主动推送（后台轮次的分条）没有本轮入站消息，骨架只给得出用户标识；那时用该用户
// 最近一次入站的 context_token，与推送本身用的是同一份票据。
func (p *Plugin) onTyping(ctx context.Context, msg imbot.Message, active bool) {
	token := msg.ReplyTo
	if token == "" {
		p.mu.Lock()
		tokens := p.tokens
		p.mu.Unlock()
		if tokens != nil {
			token = tokens.get(msg.UserID)
		}
	}
	p.setTyping(ctx, msg.UserID, token, active)
}

// startPolling （重）启动长轮询循环：先停掉旧循环，再以当前凭证起新的。
func (p *Plugin) startPolling() {
	p.mu.Lock()
	if p.pollCancel != nil {
		p.pollCancel()
		p.pollCancel = nil
	}
	if p.ctx == nil {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(p.ctx)
	p.pollCancel = cancel
	p.wg.Add(1)
	p.mu.Unlock()
	go p.pollLoop(ctx)
}

// Stop 停掉轮询循环、绑定流程与骨架的全部 worker。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel, core := p.cancel, p.core
	p.cancel = nil
	p.pollCancel = nil
	p.mu.Unlock()
	if core != nil {
		core.Stop()
	}
	if cancel != nil {
		cancel()
		p.wg.Wait()
	}
}

// handleInbound 把一条微信消息交给公共骨架。unsupported 表示消息里有暂不支持的媒体类型。
func (p *Plugin) handleInbound(ctx context.Context, msg inbound, dedupKey string, unsupported bool) {
	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return
	}
	note := ""
	if unsupported {
		note = "暂不支持该消息类型，请发文字或语音。"
	}
	core.Handle(ctx, imbot.Message{
		UserID:  msg.userID,
		DedupID: dedupKey,
		ReplyTo: msg.contextToken,
		Text:    msg.text,
		Note:    note,
	})
}

// sender 把骨架的发送请求接到微信的协议层。
type sender struct{ p *Plugin }

func (s sender) Send(ctx context.Context, userID, text, replyTo string) {
	s.p.send(ctx, userID, text, replyTo)
}
