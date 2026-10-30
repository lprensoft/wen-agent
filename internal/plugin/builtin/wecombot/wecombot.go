// Package wecombot 通过企业微信智能机器人（长连接模式）提供单聊远程会话。
//
// 协议是 WebSocket：连上 wss://openws.work.weixin.qq.com 后先发 aibot_subscribe
// 用 BotID + Secret 认证，之后企业微信在同一条连接上推 aibot_msg_callback，我们用
// 透传的 req_id 发 aibot_respond_msg（stream 消息，finish=true）回复；心跳是每 30
// 秒一条 ping。后台轮次与定时提醒没有可回带的 req_id，用 aibot_send_msg 主动推送
// markdown（单聊的 chatid 就是 userid），前提是用户先给机器人发过消息。
//
// 会话绑定、命令集（/new /status /compact /help /apply /deny）、串行处理与确认
// 代理都在公共骨架 wen/internal/imbot 里，这个包只负责企业微信的协议层。
//
// 入站消息先进入插件级 FIFO，由独立的分发协程按到达顺序处理；读循环只收帧与回执，
// 不会被回复等待拖住。某段回复的回执超时、或等待回执时被取消，这个 req_id 都不再复用（后续
// 段落改走主动推送），否则迟到的回执会被当成下一段的回执。
//
// 仅响应白名单内的 userid；白名单为空时拒绝所有人（陌生 userid 记录到日志，
// 方便抄进白名单）。仅处理单聊：群聊里 @ 机器人的消息忽略。
package wecombot

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
	defGatewayURL = "wss://openws.work.weixin.qq.com"

	formatMarkdown = "markdown"
	formatPlain    = "plain"

	// replyWindow 是收到消息后还能回复的窗口。官方限制 24 小时，扣留消息要给
	// 回复留出余量，和 QQ 的处理一致。
	replyWindow = 24 * time.Hour
)

// Plugin 是企业微信智能机器人插件。有状态：Init 可重入（先停旧连接），字段由 mu 保护。
type Plugin struct {
	mu sync.Mutex

	// 配置
	lang       string // 进程语言。本插件不给模型任何东西，文案一律走界面语言包
	botID      string
	secret     string
	gatewayURL string
	whitelist  map[string]bool
	format     string // formatMarkdown / formatPlain

	core *imbot.Core // 通道无关的分发、命令、会话绑定与确认代理
	conn *wsConn     // 当前长连接；未连上时为 nil

	callbacks *callbackQueue // 入站消息的 FIFO，保持读循环收到的顺序

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// 测试可缩短心跳间隔、回执等待时长，生产环境用默认值。
	heartbeatEvery time.Duration
	heartbeatWait  time.Duration
	ackWait        time.Duration

	// startGateway 可被测试替换，避免单测真的去连企业微信。
	startGateway func(context.Context)
}

func New() *Plugin {
	imbot.Declare("wecom_bot", "企业微信")
	p := &Plugin{
		heartbeatEvery: heartbeatInterval,
		heartbeatWait:  heartbeatTimeout,
		ackWait:        defaultAckTimeout,
	}
	p.startGateway = p.gatewayLoop
	return p
}

func (p *Plugin) Name() string { return "wecom_bot" }

func (p *Plugin) Category() string { return plugin.CategoryChannel }

func (p *Plugin) Description() string {
	return "接入企业微信智能机器人实现单聊远程会话，支持 /new /status /compact /help 命令与危险操作远程确认"
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
			Key: "bot_id", Label: "BotID", Type: plugin.FieldString, Default: "",
			Description: "企业微信管理后台 → 智能机器人 → 开启「API 模式」并选择「长连接」后拿到的机器人 ID",
		},
		{
			Key: "secret", Label: "Secret", Type: plugin.FieldString, Default: "",
			Description: "长连接专用的密钥（与消息回调地址模式的 Token / EncodingAESKey 不是一回事）。只保存在本机，不会上传到任何地方",
		},
		imbot.WhitelistField("userid 白名单",
			"每行一个企业微信 userid，只响应名单内用户；留空拒绝所有人。陌生用户首次发消息时其 userid 会记录到运行日志。"+
				"若机器人创建者不是企业超级管理员，这里拿到的是加密 userid"),
		{
			Key: "gateway_url", Label: "长连接地址", Type: plugin.FieldString, Default: defGatewayURL,
			Description: "一般不需要改。企业微信智能机器人的 WebSocket 长连接地址",
		},
		imbot.ConfirmTimeoutField(imbot.DefaultConfirmTimeoutSec),
		{
			Key: "format", Label: "消息格式", Type: plugin.FieldSelect, Default: formatMarkdown,
			Options: []plugin.ConfigOption{
				{Value: formatMarkdown, Label: "markdown（推荐）"},
				{Value: formatPlain, Label: "纯文本"},
			},
			Description: "markdown：按企业微信的 markdown 消息发送，客户端会渲染加粗、代码块等格式；纯文本：发送前转成可读纯文本",
		},
	}
	fields = append(fields, imbot.OutputFields()...)
	// 收发节奏的几项与别的通道共用一份声明，见 imbot.PaceFields
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
	botID := strings.TrimSpace(plugin.CfgString(cfg, "bot_id", ""))
	secret := strings.TrimSpace(plugin.CfgString(cfg, "secret", ""))
	if botID == "" || secret == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.wecom_bot.err.no_credentials",
			"请先在配置中填写 BotID 与 Secret（企业微信管理后台 → 智能机器人 → 长连接）"))
	}
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.wecom_bot.err.no_state_dir",
			"没有可用的持久化目录"))
	}
	if err := os.MkdirAll(ictx.StateDir, 0o755); err != nil {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.wecom_bot.err.mkdir",
			"创建状态目录失败: %w"), err)
	}

	whitelist := imbot.ParseWhitelist(cfg)

	p.Stop()

	p.mu.Lock()
	p.lang = ictx.Lang
	p.botID = botID
	p.secret = secret
	p.gatewayURL = strings.TrimSpace(plugin.CfgString(cfg, "gateway_url", defGatewayURL))
	p.whitelist = whitelist
	p.format = plugin.CfgString(cfg, "format", formatMarkdown)
	p.conn = nil
	p.mu.Unlock()

	core, err := imbot.NewCore(imbot.CoreOptions{
		PluginName: p.Name(),
		Sender:     sender{p: p},
		Ctx:        ictx,
		Cfg:        cfg,
		Allow:      p.allowed,
		Push:       p.push,
		// 回复窗口 24 小时，扣留要给一轮对话留出余量
		ReplyWindow: replyWindow - imbot.DefaultTurnTimeout - 5*time.Minute,
	})
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.core = core
	p.callbacks = newCallbackQueue()
	p.ctx, p.cancel = context.WithCancel(context.Background())
	ctx := p.ctx
	queue := p.callbacks
	start := p.startGateway
	p.wg.Add(2)
	p.mu.Unlock()

	core.Start(ctx)
	go func() {
		defer p.wg.Done()
		p.dispatchLoop(ctx, queue)
	}()
	go func() {
		defer p.wg.Done()
		start(ctx)
	}()
	return nil
}

// allowed 判定用户是否放行：只看配置白名单，空名单拒绝所有人。
func (p *Plugin) allowed(userID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return userID != "" && p.whitelist[userID]
}

// Stop 断开长连接并停掉骨架的全部 worker。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel, core := p.cancel, p.core
	p.cancel = nil
	p.mu.Unlock()
	imbot.StopChannel(cancel, core, &p.wg)
}

// sender 把骨架的发送请求接到企业微信的协议层。
type sender struct{ p *Plugin }

func (s sender) Send(ctx context.Context, userID, text, replyTo string) {
	s.p.send(ctx, userID, text, replyTo)
}
