// Package qqbot 通过 QQ 官方开放平台（API v2 + WebSocket 事件网关）提供 C2C 私聊远程会话。
//
// 每个 QQ 用户（openid）映射到一个当前会话，远程会话就是普通会话（同一个会话存储），
// Web UI 刷新即可看到。会话绑定、命令集（/new /status /compact /help /apply /deny）、
// 串行处理与确认代理都在公共骨架 wen/internal/imbot 里，这个包只负责 QQ 的协议层：
// 鉴权、事件网关、被动回复额度、原生 markdown 的降级。
//
// 协议层参考官方 qqbot-nodejs SDK：被动回复带 msg_id（同一条消息 60 分钟内最多回
// 4 条，超限降级为主动消息）、入站消息按 id 去重。
//
// 仅响应白名单内的 openid；白名单为空时拒绝所有人（陌生 openid 记录到日志，
// 方便抄进白名单）。
package qqbot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/plugin"
)

const (
	// 2026-08-10 起官方把域名统一为 api.bot.qq.com，token 也改从这里取。旧的
	// api.sgroup.qq.com / bots.qq.com 仍能用、没有下线日期，存过旧值的配置不迁移
	defAPIBase        = "https://api.bot.qq.com"
	defTokenURL       = "https://api.bot.qq.com/app/getAppAccessToken"
	defConfirmTimeout = 300 // 秒

	turnTimeout = 10 * time.Minute

	formatMarkdown = "markdown"
	formatPlain    = "plain"
)

// Plugin 是 QQ 机器人插件。有状态：Init 可重入（先停旧网关连接），字段由 mu 保护。
type Plugin struct {
	mu sync.Mutex

	// 配置
	lang      string // 进程语言。本插件不给模型任何东西，文案一律走界面语言包
	appID     string
	appSecret string
	apiBase   string
	whitelist map[string]bool
	format    string // formatMarkdown / formatPlain

	// markdown 能力缓存：openid → 关闭到何时（平台返回 markdown 被拒的错误码后记入，见 isMarkdownForbidden）
	mdOff map[string]time.Time

	// 运行组件
	core    *imbot.Core // 通道无关的分发、命令、会话绑定与确认代理
	tokens  *tokenSource
	limiter *replyLimiter

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// tokenURLOverride 供测试指向本地假鉴权服务，生产环境留空。
	tokenURLOverride string
}

// inbound 是一条收到的 C2C 消息。
type inbound struct {
	openid  string
	msgID   string
	content string
}

func New() *Plugin {
	imbot.Declare("qq_bot", "QQ")
	return &Plugin{}
}

func (p *Plugin) Name() string { return "qq_bot" }

func (p *Plugin) Category() string { return plugin.CategoryChannel }

func (p *Plugin) Description() string {
	return "接入 QQ 官方机器人实现私聊远程会话，支持 /new /status /compact /help 命令与危险操作远程确认"
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
			Key: "app_id", Label: "AppID", Type: plugin.FieldString, Default: "",
			Description: "QQ 开放平台机器人的 AppID（q.qq.com 开发设置页）",
		},
		{
			Key: "app_secret", Label: "AppSecret", Type: plugin.FieldString, Default: "",
			Description: "机器人密钥，只保存在本机，不会上传到任何地方",
		},
		{
			Key: "whitelist", Label: "openid 白名单", Type: plugin.FieldText, Default: "",
			Description: "每行一个 user_openid，只响应名单内用户；留空拒绝所有人。陌生用户首次发消息时其 openid 会记录到运行日志",
		},
		{
			Key: "api_base", Label: "API 地址", Type: plugin.FieldString, Default: defAPIBase,
			Description: "一般不需要改。注意官方沙箱环境不支持单聊，私聊需正式发布后可用",
		},
		{
			Key: "confirm_timeout_sec", Label: "确认等待超时（秒）", Type: plugin.FieldInt,
			Default: defConfirmTimeout, Min: plugin.IntPtr(10), Max: plugin.IntPtr(3600),
			Description: "危险操作发出确认请求后等待 /apply 或 /deny 的时长，超时按拒绝处理",
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
		{
			Key: "format", Label: "消息格式", Type: plugin.FieldSelect, Default: formatMarkdown,
			Options: []plugin.ConfigOption{
				{Value: formatMarkdown, Label: "markdown（推荐）"},
				{Value: formatPlain, Label: "纯文本"},
			},
			Description: "markdown：按原生 markdown 消息发送（表格与图片会转成文字），平台拒绝时对该用户自动退回纯文本；纯文本：一律转成可读纯文本发送",
		},
	}
	// 收发节奏的几项与别的通道共用一份声明，见 imbot.PaceFields
	return append(fields, imbot.PaceFields()...)
}

// Init 应用配置并（重）启动网关连接。可重入：先停旧连接与骨架。
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
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.qq_bot.err.no_credentials",
			"请先在配置中填写 AppID 与 AppSecret"))
	}
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.qq_bot.err.no_state_dir",
			"没有可用的持久化目录"))
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
	p.appID, p.appSecret = appID, appSecret
	p.apiBase = strings.TrimRight(plugin.CfgString(cfg, "api_base", defAPIBase), "/")
	p.whitelist = whitelist
	p.lang = ictx.Lang
	p.format = plugin.CfgString(cfg, "format", formatMarkdown)
	p.mdOff = map[string]time.Time{}
	p.tokens = newTokenSource(appID, appSecret)
	if p.tokenURLOverride != "" {
		p.tokens.tokenURL = p.tokenURLOverride
	}
	p.limiter = newReplyLimiter()
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
		HoldWhenBusy:   holdWhenBusy,
		WakeAfter:      wakeAfter,
		Allow:          p.allowed,
		Push:           p.push,
		Notice:         ictx.Notice,
		RunTurn:        ictx.RunTurn,
		NewSession:     ictx.NewSession,
		Compact:        ictx.Compact,
		Status:         ictx.Status,
		Sessions:       ictx.Sessions,
		// 扣留要给被动回复留出余地：同一条消息 60 分钟内才能被动回复，放行后那一轮
		// 还可能跑上 turnTimeout，再留五分钟余量。超了就只能走额度有限的主动消息。
		ReplyWindow: replyWindow - turnTimeout - 5*time.Minute,
	})
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.core = core
	p.ctx, p.cancel = context.WithCancel(context.Background())
	ctx := p.ctx
	p.wg.Add(1)
	p.mu.Unlock()

	core.Start(ctx)
	go p.gatewayLoop(ctx)
	return nil
}

// push 主动推送：QQ 没有可回带的 msg_id 时走主动消息，发不出去由 send 记日志容忍，
// 所以这里恒报「已交给平台」。
func (p *Plugin) push(ctx context.Context, openid, text string) bool {
	p.send(ctx, openid, text, "")
	return true
}

// allowed 判定用户是否放行：只看配置白名单，空名单拒绝所有人。
func (p *Plugin) allowed(openid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return openid != "" && p.whitelist[openid]
}

// Stop 断开网关连接并停掉骨架的全部 worker。
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

// handleInbound 把一条 QQ 消息交给公共骨架。
func (p *Plugin) handleInbound(ctx context.Context, msg inbound) {
	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return
	}
	core.Handle(ctx, imbot.Message{
		UserID:  msg.openid,
		DedupID: msg.msgID,
		ReplyTo: msg.msgID,
		Text:    msg.content,
	})
}

// sender 把骨架的发送请求接到 QQ 的协议层。用独立类型而不是给 Plugin 加导出方法：
// Plugin 的导出方法面向插件契约，Send 不属于那套契约。
type sender struct{ p *Plugin }

func (s sender) Send(ctx context.Context, userID, text, replyTo string) {
	s.p.send(ctx, userID, text, replyTo)
}
