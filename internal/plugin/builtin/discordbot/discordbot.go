// Package discordbot 通过 Discord Bot API 提供私聊远程会话。
//
// 收消息走官方 Gateway（WebSocket）：只订阅私信与基础服务器事件，不申请
// Message Content 这个 privileged intent——官方把「发给 bot 的私信」列为例外，
// 私信正文不需要它。发消息、开私信频道与「正在输入」走 REST。
//
// 页面里的「用户标识」用 DM channel ID，而不是 Discord 的用户 ID：一条私信
// 就是一个频道，回复、输入状态与后台轮次推送都直接发给它，不需要再存一份
// 「用户 → 推送凭据」的映射；白名单也按 DM channel ID 写。
//
// 会话绑定、命令集（/new /status /compact /help /apply /deny）、串行处理与确认
// 代理都在公共骨架 wen/internal/imbot 里，这个包只负责 Discord 的协议层。
package discordbot

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/plugin"
)

const (
	defAPIBase     = "https://discord.com/api/v10"
	defGatewayURL  = "wss://gateway.discord.gg/?v=10&encoding=json"
	apiTimeout     = 30 * time.Second
	formatMarkdown = "markdown"
	formatPlain    = "plain"
)

// Plugin 是 Discord 机器人插件。有状态：Init 可重入（先停旧网关连接），字段由 mu 保护。
type Plugin struct {
	mu sync.Mutex

	// 配置
	lang       string // 进程语言。本插件不给模型任何东西，文案一律走界面语言包
	token      string
	apiBase    string
	gatewayURL string
	proxy      *url.URL // nil 表示直连（仍会读 HTTPS_PROXY 等环境变量）
	whitelist  map[string]bool
	format     string // formatMarkdown / formatPlain

	client *http.Client
	core   *imbot.Core // 通道无关的分发、命令、会话绑定与确认代理

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// startGateway 可被测试替换，避免单测真的去连 Discord。
	startGateway func(ctx context.Context)
}

func New() *Plugin {
	imbot.Declare("discord_bot", "Discord")
	p := &Plugin{}
	p.startGateway = p.gatewayLoop
	return p
}

func (p *Plugin) Name() string { return "discord_bot" }

func (p *Plugin) Category() string { return plugin.CategoryChannel }

func (p *Plugin) Description() string {
	return "接入 Discord 机器人实现私聊远程会话，支持 /new /status /compact /help 命令与危险操作远程确认"
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
			Key: "bot_token", Label: "Bot Token", Type: plugin.FieldString, Default: "",
			Description: "Discord 开发者门户 → 你的应用 → Bot 页签里复制/重置得到的令牌。只保存在本机，不会上传到任何地方",
		},
		imbot.WhitelistField("DM channel 白名单",
			"每行一个 Discord 私聊频道 ID（形如 123456789012345678）。只响应名单内频道；留空拒绝所有人。"+
				"陌生用户首次发消息时其频道 ID 会记录到运行日志，方便抄入"),
		{
			Key: "proxy", Label: "代理地址", Type: plugin.FieldString, Default: "",
			Description: "Discord 在部分网络下直连不通。支持 http / https / socks5，形如 socks5://127.0.0.1:1080 或 http://127.0.0.1:7890；留空直连（仍会读取 HTTPS_PROXY 等环境变量）",
		},
		imbot.APIBaseField(defAPIBase,
			"一般不需要改。Discord REST API 地址"),
		{
			Key: "gateway_url", Label: "Gateway 地址", Type: plugin.FieldString, Default: defGatewayURL,
			Description: "一般不需要改。Discord 事件网关地址（WebSocket）",
		},
		imbot.ConfirmTimeoutField(imbot.DefaultConfirmTimeoutSec),
		{
			Key: "format", Label: "消息格式", Type: plugin.FieldSelect, Default: formatMarkdown,
			Options: []plugin.ConfigOption{
				{Value: formatMarkdown, Label: "markdown（推荐）"},
				{Value: formatPlain, Label: "纯文本"},
			},
			Description: "markdown：原样发送，由 Discord 客户端渲染加粗、代码块等格式；纯文本：发送前转成可读纯文本",
		},
	}
	fields = append(fields, imbot.OutputFields()...)
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
	token := strings.TrimSpace(plugin.CfgString(cfg, "bot_token", ""))
	if token == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.discord_bot.err.no_token",
			"请先在配置中填写 Bot Token（Discord 开发者门户 → 应用 → Bot）"))
	}
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.discord_bot.err.no_state_dir",
			"没有可用的持久化目录"))
	}
	if err := os.MkdirAll(ictx.StateDir, 0o755); err != nil {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.discord_bot.err.mkdir",
			"创建状态目录失败: %w"), err)
	}

	proxyURL, transport, err := buildProxy(ictx.Lang, plugin.CfgString(cfg, "proxy", ""))
	if err != nil {
		return err
	}

	whitelist := imbot.ParseWhitelist(cfg)

	p.Stop()

	p.mu.Lock()
	p.lang = ictx.Lang
	p.token = token
	p.apiBase = strings.TrimRight(plugin.CfgString(cfg, "api_base", defAPIBase), "/")
	p.gatewayURL = strings.TrimSpace(plugin.CfgString(cfg, "gateway_url", defGatewayURL))
	p.proxy = proxyURL
	p.whitelist = whitelist
	p.format = plugin.CfgString(cfg, "format", formatMarkdown)
	p.client = &http.Client{Timeout: apiTimeout, Transport: transport}
	p.mu.Unlock()

	core, err := imbot.NewCore(imbot.CoreOptions{
		PluginName: p.Name(),
		Sender:     sender{p: p},
		Ctx:        ictx,
		Cfg:        cfg,
		Allow:      p.allowed,
		Push:       p.push,
		Typing:     p.typing,
	})
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.core = core
	p.ctx, p.cancel = context.WithCancel(context.Background())
	ctx := p.ctx
	start := p.startGateway
	p.wg.Add(1)
	p.mu.Unlock()

	core.Start(ctx)
	go func() {
		defer p.wg.Done()
		start(ctx)
	}()
	return nil
}

// allowed 判定用户是否放行：只看配置白名单，空名单拒绝所有人。
func (p *Plugin) allowed(channelID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return channelID != "" && p.whitelist[channelID]
}

// Stop 断开网关连接并停掉骨架的全部 worker。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel, core := p.cancel, p.core
	p.cancel = nil
	p.mu.Unlock()
	imbot.StopChannel(cancel, core, &p.wg)
}

// sender 把骨架的发送请求接到 Discord 的协议层。
type sender struct{ p *Plugin }

func (s sender) Send(ctx context.Context, userID, text, replyTo string) {
	s.p.send(ctx, userID, text, replyTo)
}
