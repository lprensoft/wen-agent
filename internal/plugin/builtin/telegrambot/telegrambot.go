// Package telegrambot 通过 Telegram Bot API 提供私聊远程会话。
//
// 协议是纯 HTTP：getUpdates 长轮询收消息、sendMessage 回消息，端点形如
// https://api.telegram.org/bot<token>/METHOD。会话绑定、命令集
// （/new /status /compact /help /apply /deny）、串行处理与确认代理都在公共骨架
// wen/internal/imbot 里，这个包只负责 Telegram 的协议层：长轮询与游标、分段发送、
// MarkdownV2 转义与降级、限流退避、代理。
//
// 仅响应白名单内的 chat_id；白名单为空时拒绝所有人（陌生 chat_id 记录到日志，
// 方便抄进白名单）。仅私聊：群组与频道的消息忽略。
package telegrambot

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
	defAPIBase        = "https://api.telegram.org"
	defConfirmTimeout = 300 // 秒

	turnTimeout = 10 * time.Minute

	// chunkLimit 是单条消息的 rune 上限。官方上限 4096，留出分段序号与转义的余量。
	chunkLimit = 3800

	formatMarkdown = "markdown"
	formatPlain    = "plain"
)

// Plugin 是 Telegram 机器人插件。有状态：Init 可重入（先停旧轮询），字段由 mu 保护。
type Plugin struct {
	mu sync.Mutex

	// 配置
	lang      string // 进程语言。本插件不给模型任何东西，文案一律走界面语言包
	token     string
	apiBase   string
	whitelist map[string]bool
	format    string // formatMarkdown / formatPlain

	// markdown 能力缓存：chat_id → 关闭到何时（平台报解析失败后记入）
	mdOff map[string]time.Time

	stateDir string

	// 两个 client：长轮询那个的超时必须大于长轮询窗口，否则每轮都在自己这边超时
	client     *http.Client
	pollClient *http.Client

	core *imbot.Core // 通道无关的分发、命令、会话绑定与确认代理

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New() *Plugin {
	imbot.Declare("telegram_bot", "Telegram")
	return &Plugin{}
}

func (p *Plugin) Name() string { return "telegram_bot" }

func (p *Plugin) Category() string { return plugin.CategoryChannel }

func (p *Plugin) Description() string {
	return "接入 Telegram 机器人实现私聊远程会话，支持 /new /status /compact /help 命令与危险操作远程确认"
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
			Description: "找 @BotFather 用 /newbot 建一个机器人拿到的令牌，形如 123456:ABC-DEF…。只保存在本机，不会上传到任何地方",
		},
		{
			Key: "whitelist", Label: "chat_id 白名单", Type: plugin.FieldText, Default: "",
			Description: "每行一个 chat_id（私聊时等于你的用户 ID，数字）。只响应名单内用户；留空拒绝所有人。给机器人发一条消息后，其 chat_id 会记录到运行日志",
		},
		{
			Key: "proxy", Label: "代理地址", Type: plugin.FieldString, Default: "",
			Description: "api.telegram.org 在部分网络下直连不通。支持 http / https / socks5，形如 socks5://127.0.0.1:1080 或 http://127.0.0.1:7890；留空直连",
		},
		{
			Key: "api_base", Label: "API 地址", Type: plugin.FieldString, Default: defAPIBase,
			Description: "一般不需要改。自建 Bot API 服务端或反向代理时填这里",
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
			Description: "markdown：转成 Telegram 的 MarkdownV2 发送，加粗、代码块等照常渲染，解析失败时对该用户自动退回纯文本；纯文本：一律转成可读纯文本发送",
		},
		{
			Key: "stream_preview", Label: "边写边显示", Type: plugin.FieldBool, Default: false,
			Description: "回复还在生成时，在对话里显示正在写的内容（Telegram 的草稿预览，写完后换成正式消息）。" +
				"开着就不像真人打字了：真人不会让你看着一个字一个字往外冒",
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

// Init 应用配置并（重）启动长轮询。可重入：先停旧循环与骨架。
// langOf 取进程语言。
func (p *Plugin) langOf() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	token := strings.TrimSpace(plugin.CfgString(cfg, "bot_token", ""))
	if token == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.telegram_bot.err.no_token",
			"请先在配置中填写 Bot Token（找 @BotFather 建机器人获取）"))
	}
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.telegram_bot.err.no_state_dir",
			"没有可用的持久化目录"))
	}
	// 目录按约定可能还不存在。收信游标是第一个落盘的东西（比会话绑定还早），
	// 不先建出来的话首条消息的游标就存不下，重启会重收一遍
	if err := os.MkdirAll(ictx.StateDir, 0o755); err != nil {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.telegram_bot.err.mkdir",
			"创建状态目录失败: %w"), err)
	}

	transport, err := buildTransport(ictx.Lang, strings.TrimSpace(plugin.CfgString(cfg, "proxy", "")))
	if err != nil {
		return err
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
	p.token = token
	p.apiBase = strings.TrimRight(plugin.CfgString(cfg, "api_base", defAPIBase), "/")
	p.whitelist = whitelist
	p.lang = ictx.Lang
	p.format = plugin.CfgString(cfg, "format", formatMarkdown)
	p.mdOff = map[string]time.Time{}
	p.stateDir = ictx.StateDir
	p.client = &http.Client{Timeout: apiTimeout, Transport: transport}
	// 长轮询这个的超时要盖过服务端的等待窗口，否则每一轮都会在客户端先断
	p.pollClient = &http.Client{Timeout: longPollTimeout + 30*time.Second, Transport: transport}
	p.mu.Unlock()

	var preview func(context.Context, imbot.Message, string)
	if plugin.CfgBool(cfg, "stream_preview", false) {
		preview = p.onPreview
	}
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
		Typing:         p.onTyping,
		Preview:        preview,
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
	p.wg.Add(1)
	p.mu.Unlock()

	core.Start(ctx)
	go p.pollLoop(ctx)
	return nil
}

// buildTransport 按代理配置造 Transport。空配置返回 nil（用默认 Transport，
// 它会读 HTTPS_PROXY 之类的环境变量）。
//
// http / https / socks5 / socks5h 都由标准库原生支持，不需要额外依赖。
//
// 返回接口而不是 *http.Transport：后者的 nil 值赋进 http.Client.Transport 会变成
// 「非 nil 接口包着 nil 指针」，标准库照着解引用当场 panic。
func buildTransport(lang, proxy string) (http.RoundTripper, error) {
	if proxy == "" {
		return nil, nil
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.telegram_bot.err.proxy_parse",
			"代理地址无法解析: %w"), err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	case "":
		return nil, fmt.Errorf("%s", i18n.T(lang, "plugin.telegram_bot.err.proxy_scheme",
			"代理地址要带协议前缀，如 socks5://127.0.0.1:1080"))
	default:
		return nil, fmt.Errorf(i18n.T(lang, "plugin.telegram_bot.err.proxy_unsupported",
			"不支持的代理协议 %q，可用 http、https、socks5"), u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%s", i18n.T(lang, "plugin.telegram_bot.err.proxy_no_host",
			"代理地址缺少主机和端口"))
	}
	return &http.Transport{Proxy: http.ProxyURL(u)}, nil
}

// allowed 判定用户是否放行：只看配置白名单，空名单拒绝所有人。
func (p *Plugin) allowed(chatID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return chatID != "" && p.whitelist[chatID]
}

// onTyping 把骨架的「正在输入」回调接到 Telegram 的 sendChatAction。
func (p *Plugin) onTyping(ctx context.Context, msg imbot.Message, active bool) {
	if !active {
		return // Telegram 的输入状态 5 秒自动消失，不需要显式关闭
	}
	p.sendChatAction(ctx, msg.UserID)
}

// Stop 停掉长轮询与骨架的全部 worker。
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

// sender 把骨架的发送请求接到 Telegram 的协议层。
type sender struct{ p *Plugin }

func (s sender) Send(ctx context.Context, userID, text, replyTo string) {
	s.p.send(ctx, userID, text, replyTo)
}
