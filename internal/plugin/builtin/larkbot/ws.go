package larkbot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"wen/internal/imbot"
	"wen/internal/llm"

	"wen/internal/i18n"
)

// runWS 起长连接。SDK 的 Start 会阻塞并自带重连，ctx 取消即退出（v3.11.0 起才如此：
// 更早的版本连上之后是一个不看 ctx 的 select{}，见 ws_test.go）。
//
// 长连接的线格式是 protobuf 私有协议，官方不公开，只能用 SDK——这是本项目里
// 唯一一处「协议层不自己写」的地方。发消息仍是手写 HTTP。
func (p *Plugin) runWS(ctx context.Context, appID, appSecret, base string) {
	handler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, ev *larkim.P2MessageReceiveV1) error {
			// 刻意不用回调带进来的 ctx：它属于这一次事件投递，而处理里可能要跑
			// 一整轮对话，用它会在轮次跑完之前被取消
			p.onMessage(ev)
			return nil
		})

	cli := larkws.NewClient(appID, appSecret,
		larkws.WithEventHandler(handler),
		larkws.WithDomain(base),
		larkws.WithAutoReconnect(true),
		larkws.WithLogLevel(larkcore.LogLevelWarn),
	)

	p.mu.Lock()
	p.wg.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.wg.Done()
		i18n.Logf(p.langOf(), "log.lark.connecting", "%s: 正在建立长连接（%s）", p.Name(), base)
		if err := cli.Start(ctx); err != nil && ctx.Err() == nil {
			i18n.Logf(p.langOf(), "log.lark.disconnected", "%s: 长连接结束: %v。请确认开发者后台已把事件订阅方式设为「长连接」，"+
				"并订阅了「接收消息」事件、开通了 im:message.p2p_msg:readonly（收单聊消息）与 im:message:send_as_bot 权限",
				p.Name(), err)
		}
	}()
}

// mentionRe 匹配正文里的 @ 占位符（形如 @_user_1）。单聊本来不需要 @，
// 但用户手滑带上时，占位符原样喂给模型只会造成困惑。
var mentionRe = regexp.MustCompile(`@_user_\d+\s*`)

// onMessage 把一条事件消息转成 imbot.Message 交给公共骨架。
func (p *Plugin) onMessage(ev *larkim.P2MessageReceiveV1) {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil {
		return
	}
	msg := ev.Event.Message

	// 机器人自己发的消息会回显，处理它就成了自己跟自己聊
	if ev.Event.Sender == nil || deref(ev.Event.Sender.SenderType) != "user" {
		return
	}
	openID := ""
	if ev.Event.Sender.SenderId != nil {
		openID = deref(ev.Event.Sender.SenderId.OpenId)
	}
	if openID == "" {
		return
	}
	if ct := deref(msg.ChatType); ct != "p2p" {
		i18n.Logf(p.langOf(), "log.lark.group_ignored", "%s: 忽略非单聊消息（chat_type=%s，仅支持单聊）", p.Name(), ct)
		return
	}

	messageID := deref(msg.MessageId)
	text, note, imageKey := parseContent(p.langOf(), deref(msg.MessageType), deref(msg.Content))

	p.mu.Lock()
	core, runCtx := p.core, p.ctx
	p.mu.Unlock()
	if core == nil || runCtx == nil {
		return
	}
	images, imageNote := p.inboundImages(runCtx, messageID, imageKey)
	if note == "" {
		note = imageNote
	}
	core.Handle(runCtx, imbot.Message{
		UserID:  openID,
		DedupID: messageID,
		ReplyTo: messageID,
		Text:    text,
		Images:  images,
		Note:    note,
	})
}

// parseContent 从事件的 content（JSON 字符串）里取出正文或图片 key。
// 只认文本与图片，其余类型回一句暂不支持。
func parseContent(lang, msgType, content string) (text, note, imageKey string) {
	switch msgType {
	case "text":
		var c struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(content), &c); err != nil {
			return "", i18n.T(lang, "plugin.lark_bot.msg.parse_failed", "消息内容解析失败，请重发。"), ""
		}
		text = strings.TrimSpace(mentionRe.ReplaceAllString(c.Text, ""))
		if text == "" {
			return "", i18n.T(lang, "plugin.lark_bot.msg.no_text", "没有收到文字内容。"), ""
		}
		return text, "", ""
	case "image":
		var c struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(content), &c); err != nil || c.ImageKey == "" {
			return "", i18n.T(lang, "plugin.lark_bot.msg.parse_failed", "消息内容解析失败，请重发。"), ""
		}
		return "", "", c.ImageKey
	default:
		return "", i18n.T(lang, "plugin.lark_bot.msg.unsupported", "暂不支持该消息类型，请发文字。"), ""
	}
}

// inboundImages 按消息资源接口下载图片；飞书的下载地址需要 tenant token。
func (p *Plugin) inboundImages(ctx context.Context, messageID, imageKey string) ([]llm.ImagePart, string) {
	if imageKey == "" {
		return nil, ""
	}
	if messageID == "" {
		return nil, i18n.T(p.langOf(), "plugin.lark_bot.msg.image_failed", "图片下载失败，请重发。")
	}
	p.mu.Lock()
	base, tokens, client := p.apiBase, p.tokens, p.client
	p.mu.Unlock()
	if tokens == nil || client == nil {
		return nil, i18n.T(p.langOf(), "plugin.lark_bot.msg.image_failed", "图片下载失败，请重发。")
	}
	token, err := tokens.Get(ctx)
	if err != nil {
		i18n.Logf(p.langOf(), "log.lark.image_failed", "%s: 获取 token 失败: %v", p.Name(), err)
		return nil, i18n.T(p.langOf(), "plugin.lark_bot.msg.image_failed", "图片下载失败，请重发。")
	}
	url := base + "/open-apis/im/v1/messages/" + messageID + "/resources/" + imageKey + "?type=image"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, i18n.T(p.langOf(), "plugin.lark_bot.msg.image_failed", "图片下载失败，请重发。")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		i18n.Logf(p.langOf(), "log.lark.image_failed", "%s: 下载图片失败: %v", p.Name(), err)
		return nil, i18n.T(p.langOf(), "plugin.lark_bot.msg.image_failed", "图片下载失败，请重发。")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		i18n.Logf(p.langOf(), "log.lark.image_failed", "%s: 下载图片失败（HTTP %d）", p.Name(), resp.StatusCode)
		return nil, i18n.T(p.langOf(), "plugin.lark_bot.msg.image_failed", "图片下载失败，请重发。")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, imbot.MaxImageBytes+1))
	if err != nil || int64(len(data)) > imbot.MaxImageBytes {
		return nil, i18n.T(p.langOf(), "plugin.lark_bot.msg.image_too_large", "图片超过大小上限，请压缩后再发。")
	}
	img, err := imbot.ImageFromBytes(data)
	if err != nil {
		i18n.Logf(p.langOf(), "log.lark.image_invalid", "%s: 图片无效（已跳过）: %v", p.Name(), err)
		return nil, i18n.T(p.langOf(), "plugin.lark_bot.msg.image_failed", "图片下载失败，请重发。")
	}
	return []llm.ImagePart{img}, ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
