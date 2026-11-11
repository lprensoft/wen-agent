// Package dingtalkbot 通过钉钉 Stream 模式提供单聊远程会话。
//
// 收消息走 Stream 长连接：向 /v1.0/gateway/connections/open 换取 WebSocket 接入点
// 与 ticket，订阅机器人消息回调 /v1.0/im/bot/messages/get；每条回调都要回一个
// 带相同 messageId 的 ack，否则服务端会重推。入站消息按到达顺序进入公共队列，
// 读循环只负责入队、回 ack 与控制帧。
//
// 发消息与后台推送都走 OpenAPI POST /v1.0/robot/oToMessages/batchSend（应用
// AccessToken + robotCode），而不是回调里的 sessionWebhook：后者只有约 30 秒
// 有效期，一轮对话常常跑得比它久。主动推送要求用户先给机器人发过消息。
//
// 会话绑定、命令集（/new /status /compact /help /apply /deny）、串行处理与确认
// 代理都在公共骨架 wen/internal/imbot 里，这个包只负责钉钉的协议层。
//
// 仅响应白名单内的 userid（senderStaffId）；白名单为空时拒绝所有人。仅处理
// 单聊：群聊 @ 机器人的消息忽略。
package dingtalkbot

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/llm"
	"wen/internal/plugin"
)

const (
	defAPIBase = "https://api.dingtalk.com"

	formatMarkdown = "markdown"
	formatPlain    = "plain"

	apiTimeout = 30 * time.Second
)

// Plugin 是钉钉机器人插件。有状态：Init 可重入（先停旧连接），字段由 mu 保护。
type Plugin struct {
	mu sync.Mutex

	// 配置
	lang      string // 进程语言。本插件不给模型任何东西，文案一律走界面语言包
	appKey    string
	appSecret string
	robotCode string
	apiBase   string
	oapiBase  string // 老 OpenAPI（media/upload）所在域，测试可指向本地假服务
	whitelist map[string]bool
	format    string // formatMarkdown / formatPlain

	client *http.Client // OpenAPI 与 Stream 接入点共用
	tokens *tokenSource

	core  *imbot.Core
	queue *imbot.OrderedQueue[callbackJob] // 入站消息 FIFO，保持读循环收到的顺序

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// startStream 可被测试替换。
	startStream func(context.Context)
}

func New() *Plugin {
	imbot.Declare("dingtalk_bot", "钉钉")
	p := &Plugin{}
	p.startStream = p.streamLoop
	return p
}

func (p *Plugin) Name() string { return "dingtalk_bot" }

func (p *Plugin) Category() string { return plugin.CategoryChannel }

func (p *Plugin) Description() string {
	return "接入钉钉机器人实现单聊远程会话，支持 /new /status /compact /help 命令与危险操作远程确认"
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
			Key: "app_key", Label: "Client ID（AppKey）", Type: plugin.FieldString, Default: "",
			Description: "钉钉开发者后台 → 应用详情 → 凭证与基础信息里的 Client ID（原 AppKey）",
		},
		{
			Key: "app_secret", Label: "Client Secret（AppSecret）", Type: plugin.FieldString, Default: "",
			Description: "同一个页面里的 Client Secret（原 AppSecret）。只保存在本机，不会上传到任何地方",
		},
		{
			Key: "robot_code", Label: "RobotCode", Type: plugin.FieldString, Default: "",
			Description: "应用详情 → 机器人与消息推送里的机器人编码，发消息需要它",
		},
		imbot.WhitelistField("userid 白名单",
			"每行一个钉钉 userid（回调里的 senderStaffId），只响应名单内用户；留空拒绝所有人。"+
				"陌生用户首次发消息时其 userid 会记录到运行日志。机器人发布上线后才会返回 senderStaffId"),
		imbot.APIBaseField(defAPIBase,
			"一般不需要改。钉钉 OpenAPI 与 Stream 接入点地址"),
		imbot.ConfirmTimeoutField(imbot.DefaultConfirmTimeoutSec),
		{
			Key: "format", Label: "消息格式", Type: plugin.FieldSelect, Default: formatMarkdown,
			Options: []plugin.ConfigOption{
				{Value: formatMarkdown, Label: "markdown（推荐）"},
				{Value: formatPlain, Label: "纯文本"},
			},
			Description: "markdown：按钉钉的 sampleMarkdown 消息发送，客户端会渲染加粗、代码块等格式；纯文本：发送前转成可读纯文本",
		},
	}
	fields = append(fields, imbot.OutputFields()...)
	// 收发节奏的几项与别的通道共用一份声明，见 imbot.PaceFields
	return append(fields, imbot.PaceFields()...)
}

// Init 应用配置并（重）启动 Stream 连接。可重入：先停旧连接与骨架。
// langOf 取进程语言。
func (p *Plugin) langOf() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	appKey := strings.TrimSpace(plugin.CfgString(cfg, "app_key", ""))
	appSecret := strings.TrimSpace(plugin.CfgString(cfg, "app_secret", ""))
	robotCode := strings.TrimSpace(plugin.CfgString(cfg, "robot_code", ""))
	if appKey == "" || appSecret == "" || robotCode == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.dingtalk_bot.err.no_credentials",
			"请先在配置中填写 Client ID、Client Secret 与 RobotCode（钉钉开发者后台 → 应用详情）"))
	}
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.dingtalk_bot.err.no_state_dir",
			"没有可用的持久化目录"))
	}
	if err := os.MkdirAll(ictx.StateDir, 0o755); err != nil {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.dingtalk_bot.err.mkdir",
			"创建状态目录失败: %w"), err)
	}

	whitelist := imbot.ParseWhitelist(cfg)

	p.Stop()

	p.mu.Lock()
	p.lang = ictx.Lang
	p.appKey = appKey
	p.appSecret = appSecret
	p.robotCode = robotCode
	p.apiBase = strings.TrimRight(plugin.CfgString(cfg, "api_base", defAPIBase), "/")
	if p.oapiBase == "" {
		p.oapiBase = defOAPIBase
	}
	p.whitelist = whitelist
	p.format = plugin.CfgString(cfg, "format", formatMarkdown)
	p.client = &http.Client{Timeout: apiTimeout}
	p.tokens = newTokenSource(p.client, p.apiBase, appKey, appSecret)
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
	start := p.startStream
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

// Stop 断开 Stream 并停掉骨架的全部 worker。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel, core := p.cancel, p.core
	p.cancel = nil
	p.mu.Unlock()
	imbot.StopChannel(cancel, core, &p.wg)
}

// sender 把骨架的发送请求接到钉钉的 OpenAPI。
type sender struct{ p *Plugin }

func (s sender) Send(ctx context.Context, userID, text, replyTo string) {
	s.p.send(ctx, userID, text)
}

func (s sender) SendImage(ctx context.Context, userID, replyTo string, img llm.ImagePart) error {
	return s.p.sendImage(ctx, userID, replyTo, img)
}

func (s sender) SendFile(ctx context.Context, userID, replyTo string, f llm.FilePart) error {
	return s.p.sendFile(ctx, userID, replyTo, f)
}
