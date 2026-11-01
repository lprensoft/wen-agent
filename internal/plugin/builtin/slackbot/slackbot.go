// Package slackbot 通过 Slack Socket Mode 提供私聊远程会话。
//
// 收消息走 Socket Mode：用 App-Level Token 调 apps.connections.open 换取 WebSocket
// 地址，连上后逐条 ack 带 envelope_id 的事件；Slack 每隔几小时会发 disconnect
// （warning / refresh_requested），按正常断线重连。发消息走 Web API chat.postMessage
// （Bot Token）。私聊靠 message.im 事件，需 im:history；App Home 的 Messages 页必须
// 打开，否则用户私聊不到机器人。
//
// 页面里的「用户标识」用 DM channel ID：一条私信就是一个 D 开头的频道，回复与
// 后台推送都直接发给它，不需要再存「用户 → 推送凭据」的映射；白名单也按这个 ID 写。
//
// 会话绑定、命令集（/new /status /compact /help /apply /deny）、串行处理与确认
// 代理都在公共骨架 wen/internal/imbot 里，这个包只负责 Slack 的协议层。
//
// 仅响应白名单内的 DM channel ID；白名单为空时拒绝所有人。仅处理单聊：频道与
// 群组消息忽略。
package slackbot

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
	defAPIBase = "https://slack.com/api"

	formatMarkdown = "markdown"
	formatPlain    = "plain"

	apiTimeout = 30 * time.Second
)

// Plugin 是 Slack 机器人插件。有状态：Init 可重入（先停旧连接），字段由 mu 保护。
type Plugin struct {
	mu sync.Mutex

	// 配置
	lang      string // 进程语言。本插件不给模型任何东西，文案一律走界面语言包
	appToken  string // xapp-，Socket Mode 建连
	botToken  string // xoxb-，chat.postMessage
	apiBase   string
	whitelist map[string]bool
	format    string // formatMarkdown / formatPlain
	proxy     *url.URL

	client *http.Client

	core  *imbot.Core
	queue *imbot.OrderedQueue[callbackJob] // 入站消息 FIFO，保持读循环收到的顺序

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// startSocket 可被测试替换。
	startSocket func(context.Context)
}

func New() *Plugin {
	imbot.Declare("slack_bot", "Slack")
	p := &Plugin{}
	p.startSocket = p.socketLoop
	return p
}

func (p *Plugin) Name() string { return "slack_bot" }

func (p *Plugin) Category() string { return plugin.CategoryChannel }

func (p *Plugin) Description() string {
	return "接入 Slack 机器人实现私聊远程会话，支持 /new /status /compact /help 命令与危险操作远程确认"
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
			Key: "app_token", Label: "App-Level Token", Type: plugin.FieldString, Default: "",
			Description: "Slack 应用 Basic Information → App-Level Tokens 生成，需带 connections:write，形如 xapp-…。只保存在本机，不会上传到任何地方",
		},
		{
			Key: "bot_token", Label: "Bot Token", Type: plugin.FieldString, Default: "",
			Description: "OAuth & Permissions 安装应用后得到的 Bot User OAuth Token，形如 xoxb-…。需要 chat:write 与 im:history。只保存在本机，不会上传到任何地方",
		},
		imbot.WhitelistField("DM channel 白名单",
			"每行一个 Slack 私聊频道 ID（D 开头）。只响应名单内频道；留空拒绝所有人。"+
				"陌生用户首次发消息时其 channel ID 会记录到运行日志，方便抄入"),
		{
			Key: "proxy", Label: "代理地址", Type: plugin.FieldString, Default: "",
			Description: "Slack 在部分网络下直连不通。支持 http / socks5，形如 socks5://127.0.0.1:1080 或 http://127.0.0.1:7890；留空直连（仍会读取 HTTPS_PROXY 等环境变量）。WebSocket 与 HTTP 同限",
		},
		imbot.APIBaseField(defAPIBase,
			"一般不需要改。Slack Web API 地址"),
		imbot.ConfirmTimeoutField(imbot.DefaultConfirmTimeoutSec),
		{
			Key: "format", Label: "消息格式", Type: plugin.FieldSelect, Default: formatMarkdown,
			Options: []plugin.ConfigOption{
				{Value: formatMarkdown, Label: "markdown（推荐）"},
				{Value: formatPlain, Label: "纯文本"},
			},
			Description: "markdown：转成 Slack mrkdwn（粗体、链接、代码块等）发送；纯文本：转成可读纯文本并关闭 Slack 的格式解析",
		},
	}
	fields = append(fields, imbot.OutputFields()...)
	// 收发节奏的几项与别的通道共用一份声明，见 imbot.PaceFields
	return append(fields, imbot.PaceFields()...)
}

// Init 应用配置并（重）启动 Socket Mode 连接。可重入：先停旧连接与骨架。
// langOf 取进程语言。
func (p *Plugin) langOf() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	appToken := strings.TrimSpace(plugin.CfgString(cfg, "app_token", ""))
	botToken := strings.TrimSpace(plugin.CfgString(cfg, "bot_token", ""))
	if appToken == "" || botToken == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.slack_bot.err.no_credentials",
			"请先在配置中填写 App-Level Token 与 Bot Token（Slack 应用 Basic Information / OAuth & Permissions）"))
	}
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.slack_bot.err.no_state_dir",
			"没有可用的持久化目录"))
	}
	if err := os.MkdirAll(ictx.StateDir, 0o755); err != nil {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.slack_bot.err.mkdir",
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
	p.appToken = appToken
	p.botToken = botToken
	p.apiBase = strings.TrimRight(plugin.CfgString(cfg, "api_base", defAPIBase), "/")
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
	})
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.core = core
	p.queue = imbot.NewOrderedQueue[callbackJob]()
	p.ctx, p.cancel = context.WithCancel(context.Background())
	ctx := p.ctx
	queue := p.queue
	start := p.startSocket
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
func (p *Plugin) allowed(channelID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return channelID != "" && p.whitelist[channelID]
}

// Stop 断开 Socket Mode 并停掉骨架的全部 worker。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel, core := p.cancel, p.core
	p.cancel = nil
	p.mu.Unlock()
	imbot.StopChannel(cancel, core, &p.wg)
}

// sender 把骨架的发送请求接到 Slack Web API。
type sender struct{ p *Plugin }

func (s sender) Send(ctx context.Context, userID, text, replyTo string) {
	s.p.send(ctx, userID, text)
}

// buildProxy 按代理配置造 Transport 与 WebSocket 用的代理 URL。
// gorilla/websocket 只支持 http / socks5，配置这里就限死，免得运行到建连才报错。
func buildProxy(lang, proxy string) (*url.URL, http.RoundTripper, error) {
	if proxy == "" {
		return nil, nil, nil
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return nil, nil, fmt.Errorf(i18n.T(lang, "plugin.slack_bot.err.proxy_parse",
			"代理地址无法解析: %w"), err)
	}
	switch u.Scheme {
	case "http", "socks5":
	case "":
		return nil, nil, fmt.Errorf("%s", i18n.T(lang, "plugin.slack_bot.err.proxy_scheme",
			"代理地址要带协议前缀，如 socks5://127.0.0.1:1080"))
	default:
		return nil, nil, fmt.Errorf(i18n.T(lang, "plugin.slack_bot.err.proxy_unsupported",
			"不支持的代理协议 %q，Slack 的 WebSocket 只支持 http、socks5"), u.Scheme)
	}
	if u.Host == "" {
		return nil, nil, fmt.Errorf("%s", i18n.T(lang, "plugin.slack_bot.err.proxy_no_host",
			"代理地址缺少主机和端口"))
	}
	return u, &http.Transport{Proxy: http.ProxyURL(u)}, nil
}
