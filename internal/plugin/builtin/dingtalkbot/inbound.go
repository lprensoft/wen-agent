package dingtalkbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/llm"
)

var errMediaTooLarge = errors.New("媒体超过大小上限")

// botMessage 是机器人回调 data 里分发需要的字段。文本在 text.content，
// 语音转写文本在 content.recognition，富文本在 content.richText。
type botMessage struct {
	ConversationID   string `json:"conversationId"`
	ConversationType string `json:"conversationType"`
	MsgID            string `json:"msgId"`
	SenderStaffID    string `json:"senderStaffId"`
	MsgType          string `json:"msgtype"`
	Text             struct {
		Content string `json:"content"`
	} `json:"text"`
	Content json.RawMessage `json:"content"`
}

// handleCallback 把一条机器人消息归一化成 imbot.Message。用户标识用
// senderStaffId（就是批量发送接口要的 userId），去重用 Stream 帧的 messageId。
func (p *Plugin) handleCallback(ctx context.Context, job callbackJob) {
	var msg botMessage
	if err := json.Unmarshal([]byte(job.raw), &msg); err != nil {
		i18n.Logf(p.langOf(), "log.dingtalk.callback_parse_failed", "dingtalk_bot: 无法解析消息回调（已跳过）: %v", err)
		return
	}
	if msg.ConversationType != "1" {
		i18n.Logf(p.langOf(), "log.dingtalk.group_ignored",
			"dingtalk_bot: 忽略非单聊消息（conversationType=%s，仅支持单聊）", msg.ConversationType)
		return
	}
	userID := strings.TrimSpace(msg.SenderStaffID)
	if userID == "" {
		// 机器人发布上线后才会返回 senderStaffId；没有它就既不能匹配白名单也不能发回去
		i18n.Logf(p.langOf(), "log.dingtalk.no_staff_id",
			"dingtalk_bot: 回调缺少 senderStaffId（机器人可能尚未发布上线），已忽略")
		return
	}

	text, unsupported := extractText(p.langOf(), msg)
	images, imageNote := p.inboundImages(ctx, msg)
	files, fileNote := p.inboundFiles(ctx, msg)
	note := joinNotes(imageNote, fileNote)
	if unsupported && note == "" {
		note = i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.unsupported", "暂不支持该消息类型，请发文字或语音。")
	}

	dedup := job.dedup
	if dedup == "" {
		dedup = msg.MsgID
	}

	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return
	}
	core.Handle(ctx, imbot.Message{
		UserID:  userID,
		DedupID: dedup,
		Text:    text,
		Images:  images,
		Files:   files,
		Note:    note,
	})
}

// extractText 抽取可读文本。语音用钉钉自带的 recognition 转写；富文本把文字项
// 拼起来，图片由 inboundImages 处理，其余非文字项标记为不支持。
func extractText(lang string, msg botMessage) (string, bool) {
	switch msg.MsgType {
	case "text":
		return strings.TrimSpace(msg.Text.Content), false
	case "audio":
		var c struct {
			Recognition string `json:"recognition"`
		}
		if json.Unmarshal(msg.Content, &c) == nil {
			if s := strings.TrimSpace(c.Recognition); s != "" {
				return i18n.T(lang, "plugin.dingtalk_bot.voice_prefix", "【语音】") + s, false
			}
		}
		return "", true
	case "picture", "file":
		return "", false
	case "richText":
		var c struct {
			RichText []richTextItem `json:"richText"`
		}
		if json.Unmarshal(msg.Content, &c) != nil {
			return "", true
		}
		var parts []string
		unsupported := false
		for _, item := range c.RichText {
			switch {
			case item.Text != "":
				if s := strings.TrimSpace(item.Text); s != "" {
					parts = append(parts, s)
				}
			case item.pictureCode() != "": // 由 inboundImages 处理
			default:
				unsupported = true
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n")), unsupported
	default:
		return "", true
	}
}

// richTextItem 兼容两种图片结构：官方示例里的嵌套 picture，以及部分客户端
// 直接给在项上的扁平 downloadCode。
type richTextItem struct {
	Text    string `json:"text"`
	Picture struct {
		DownloadCode string `json:"downloadCode"`
	} `json:"picture"`
	DownloadCode string `json:"downloadCode"`
}

func (i richTextItem) pictureCode() string {
	if i.Picture.DownloadCode != "" {
		return i.Picture.DownloadCode
	}
	return i.DownloadCode
}

// inboundImages 用回调里的 downloadCode 换临时下载链接，下载并校验图片。
func (p *Plugin) inboundImages(ctx context.Context, msg botMessage) ([]llm.ImagePart, string) {
	var codes []string
	switch msg.MsgType {
	case "picture":
		var c struct {
			DownloadCode string `json:"downloadCode"`
		}
		if json.Unmarshal(msg.Content, &c) == nil {
			codes = append(codes, c.DownloadCode)
		}
	case "richText":
		var c struct {
			RichText []richTextItem `json:"richText"`
		}
		if json.Unmarshal(msg.Content, &c) == nil {
			for _, item := range c.RichText {
				if code := item.pictureCode(); code != "" {
					codes = append(codes, code)
				}
			}
		}
	default:
		return nil, ""
	}

	var out []llm.ImagePart
	total := 0
	tooMany := false
	for _, code := range codes {
		if code == "" {
			continue
		}
		if len(out) >= imbot.MaxImages {
			tooMany = true
			continue
		}
		data, err := p.downloadImage(ctx, code)
		if err != nil {
			if errors.Is(err, errMediaTooLarge) {
				return out, i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.image_too_large",
					"图片超过大小上限，请压缩后再发。")
			}
			i18n.Logf(p.langOf(), "log.dingtalk.image_failed", "dingtalk_bot: 下载图片失败: %v", err)
			return out, i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.image_failed", "图片下载失败，请重发。")
		}
		if total+len(data) > imbot.MaxImageTotal {
			return out, i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.image_too_large",
				"图片超过大小上限，请压缩后再发。")
		}
		img, err := imbot.ImageFromBytes(data)
		if err != nil {
			i18n.Logf(p.langOf(), "log.dingtalk.image_invalid", "dingtalk_bot: 图片无效（已跳过）: %v", err)
			return out, i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.image_failed", "图片下载失败，请重发。")
		}
		out = append(out, img)
		total += len(data)
	}
	if tooMany {
		return out, i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.image_too_many",
			"一次最多处理 4 张图片，其余图片未发送给模型。")
	}
	return out, ""
}

// inboundFiles 用回调里的 downloadCode 换下载链接，取普通文件。文件名由回调
// 给出，缺失时用下载地址末段。
func (p *Plugin) inboundFiles(ctx context.Context, msg botMessage) ([]llm.FilePart, string) {
	if msg.MsgType != "file" {
		return nil, ""
	}
	var c struct {
		DownloadCode string `json:"downloadCode"`
		FileName     string `json:"fileName"`
	}
	if err := json.Unmarshal(msg.Content, &c); err != nil || c.DownloadCode == "" {
		return nil, i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.file_failed", "文件下载失败，请重发。")
	}
	data, err := p.downloadMedia(ctx, c.DownloadCode, imbot.MaxFileBytes)
	if err != nil {
		if errors.Is(err, errMediaTooLarge) {
			return nil, i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.file_too_large",
				"文件超过大小上限，请压缩后再发。")
		}
		i18n.Logf(p.langOf(), "log.dingtalk.file_failed", "dingtalk_bot: 下载文件失败: %v", err)
		return nil, i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.file_failed", "文件下载失败，请重发。")
	}
	f, err := imbot.FileFromBytes(data, c.FileName, "")
	if err != nil {
		i18n.Logf(p.langOf(), "log.dingtalk.file_invalid", "dingtalk_bot: 文件无效（已跳过）: %v", err)
		return nil, i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.file_failed", "文件下载失败，请重发。")
	}
	return []llm.FilePart{f}, ""
}

func (p *Plugin) downloadImage(ctx context.Context, downloadCode string) ([]byte, error) {
	return p.downloadMedia(ctx, downloadCode, imbot.MaxImageBytes)
}

func (p *Plugin) downloadMedia(ctx context.Context, downloadCode string, max int) ([]byte, error) {
	url, err := p.messageFileURL(ctx, downloadCode)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, errMediaTooLarge
	}
	return data, nil
}

func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n" + b
	}
}

// messageFileURL 把回调里的 downloadCode 换成临时下载链接。链接带时效，所以
// 收到消息就换、就下载，不在队列里久放。
func (p *Plugin) messageFileURL(ctx context.Context, downloadCode string) (string, error) {
	p.mu.Lock()
	tokens, client, base, robotCode := p.tokens, p.client, p.apiBase, p.robotCode
	p.mu.Unlock()
	body := map[string]string{"downloadCode": downloadCode, "robotCode": robotCode}

	for attempt := 0; attempt < 2; attempt++ {
		token, err := tokens.get(ctx)
		if err != nil {
			return "", err
		}
		raw, status, err := postJSON(ctx, client, base+downloadPath, token, body)
		if err != nil {
			return "", err
		}
		if status == http.StatusUnauthorized || isTokenError(raw) {
			tokens.invalidate()
			continue
		}
		if status != http.StatusOK {
			return "", apiErrorFrom(status, raw)
		}
		var resp struct {
			DownloadURL string `json:"downloadUrl"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return "", fmt.Errorf("解析下载响应失败: %w", err)
		}
		if resp.DownloadURL == "" {
			return "", fmt.Errorf("下载响应缺少 downloadUrl: %s", truncate(string(raw), 200))
		}
		return resp.DownloadURL, nil
	}
	return "", fmt.Errorf("刷新 AccessToken 后仍无法获取下载地址")
}
