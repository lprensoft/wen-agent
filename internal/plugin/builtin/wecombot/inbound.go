package wecombot

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/llm"
)

var downloadClient = &http.Client{Timeout: 30 * time.Second}

var errImageTooLarge = errors.New("图片超过大小上限")

// callbackImage 是图片回调里的下载地址与解密密钥（地址 5 分钟内有效）。
type callbackImage struct {
	URL    string `json:"url"`
	AESKey string `json:"aeskey"`
}

// callbackBody 只取分发需要的字段。各消息类型的正文结构见官方「接收消息」文档：
// 文本在 text.content，语音转写文本在 voice.content，图文混排在 mixed.msg_item。
type callbackBody struct {
	MsgID    string `json:"msgid"`
	ChatType string `json:"chattype"`
	MsgType  string `json:"msgtype"`
	From     struct {
		UserID string `json:"userid"`
	} `json:"from"`
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
	Voice struct {
		Content string `json:"content"`
	} `json:"voice"`
	Image callbackImage `json:"image"`
	Mixed struct {
		MsgItem []struct {
			MsgType string `json:"msgtype"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
			Image callbackImage `json:"image"`
		} `json:"msg_item"`
	} `json:"mixed"`
	CompatInfo *struct {
		Source string `json:"source"`
	} `json:"compat_info"`
}

// handleCallback 把一条 aibot_msg_callback 归一化成 imbot.Message：userid 作为
// 用户标识，msgid 去重，回调的 req_id 作为回复凭据原样透传。
func (p *Plugin) handleCallback(ctx context.Context, f wsFrame) {
	var body callbackBody
	if err := json.Unmarshal(f.Body, &body); err != nil {
		i18n.Logf(p.langOf(), "log.wecom.callback_parse_failed", "wecom_bot: 无法解析消息回调（已跳过）: %v", err)
		return
	}
	if body.CompatInfo != nil {
		// 智能助理调用、频道等不是真人在单聊里发来的消息
		i18n.Logf(p.langOf(), "log.wecom.compat_ignored", "wecom_bot: 忽略非普通用户消息（来源 %s）", body.CompatInfo.Source)
		return
	}
	if body.ChatType != "single" {
		i18n.Logf(p.langOf(), "log.wecom.group_ignored", "wecom_bot: 忽略群聊消息（chattype=%s，仅支持单聊）", body.ChatType)
		return
	}
	userID := strings.TrimSpace(body.From.UserID)
	if userID == "" {
		return
	}

	lang := p.langOf()
	text, unsupported := extractText(lang, body)
	images, note := p.inboundImages(ctx, body)
	if unsupported && note == "" {
		note = i18n.T(lang, "plugin.wecom_bot.msg.unsupported", "暂不支持该消息类型，请发文字或语音。")
	}

	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return
	}
	core.Handle(ctx, imbot.Message{
		UserID:  userID,
		DedupID: body.MsgID,
		ReplyTo: f.Headers.ReqID,
		Text:    text,
		Images:  images,
		Note:    note,
	})
}

// extractText 抽取可读文本。语音用企业微信自带的转写结果；图文混排把文字项拼起来，
// 图片由 inboundImages 处理，其余媒体类型标记为不支持（有文字时只处理文字，
// 没文字时骨架会回一句提示）。
func extractText(lang string, body callbackBody) (string, bool) {
	switch body.MsgType {
	case "text":
		return strings.TrimSpace(body.Text.Content), false
	case "voice":
		if s := strings.TrimSpace(body.Voice.Content); s != "" {
			return i18n.T(lang, "plugin.wecom_bot.voice_prefix", "【语音】") + s, false
		}
		return "", true
	case "image":
		return "", false
	case "mixed":
		var parts []string
		unsupported := false
		for _, item := range body.Mixed.MsgItem {
			switch item.MsgType {
			case "text":
				if s := strings.TrimSpace(item.Text.Content); s != "" {
					parts = append(parts, s)
				}
			case "image": // 由 inboundImages 处理
			default:
				unsupported = true
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n")), unsupported
	default:
		return "", true
	}
}

// inboundImages 下载并解密回调里的图片。企业微信不直接给明文，地址 5 分钟内有效，
// 所以拿到回调就下载，不做延迟处理。
func (p *Plugin) inboundImages(ctx context.Context, body callbackBody) ([]llm.ImagePart, string) {
	var list []callbackImage
	switch body.MsgType {
	case "image":
		list = append(list, body.Image)
	case "mixed":
		for _, item := range body.Mixed.MsgItem {
			if item.MsgType == "image" {
				list = append(list, item.Image)
			}
		}
	default:
		return nil, ""
	}

	var out []llm.ImagePart
	total := 0
	tooMany := false
	for _, im := range list {
		if im.URL == "" {
			continue
		}
		if len(out) >= imbot.MaxImages {
			tooMany = true
			continue
		}
		data, err := downloadImage(ctx, im.URL, im.AESKey)
		if err != nil {
			if errors.Is(err, errImageTooLarge) {
				return out, i18n.T(p.langOf(), "plugin.wecom_bot.msg.image_too_large",
					"图片超过大小上限，请压缩后再发。")
			}
			i18n.Logf(p.langOf(), "log.wecom.image_failed", "wecom_bot: 下载或解密图片失败: %v", err)
			return out, i18n.T(p.langOf(), "plugin.wecom_bot.msg.image_failed", "图片下载失败，请重发。")
		}
		if total+len(data) > imbot.MaxImageTotal {
			return out, i18n.T(p.langOf(), "plugin.wecom_bot.msg.image_too_large",
				"图片超过大小上限，请压缩后再发。")
		}
		img, err := imbot.ImageFromBytes(data)
		if err != nil {
			i18n.Logf(p.langOf(), "log.wecom.image_invalid", "wecom_bot: 图片无效（已跳过）: %v", err)
			return out, i18n.T(p.langOf(), "plugin.wecom_bot.msg.image_failed", "图片下载失败，请重发。")
		}
		out = append(out, img)
		total += len(data)
	}
	if tooMany {
		return out, i18n.T(p.langOf(), "plugin.wecom_bot.msg.image_too_many",
			"一次最多处理 4 张图片，其余图片未发送给模型。")
	}
	return out, ""
}

func downloadImage(ctx context.Context, rawURL, aesKey string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 密文比明文多最多一个填充块；超过这个长度就不必再解密了。
	limit := int64(imbot.MaxImageBytes + 2*aes.BlockSize)
	ciphertext, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(ciphertext)) > limit {
		return nil, errImageTooLarge
	}
	plain, err := decryptImage(ciphertext, aesKey)
	if err != nil {
		return nil, err
	}
	if len(plain) > imbot.MaxImageBytes {
		return nil, errImageTooLarge
	}
	return plain, nil
}

// decryptImage 按官方约定解密：aeskey 是 Base64 的 32 字节密钥，AES-256-CBC，
// IV 取密钥前 16 字节。填充是 PKCS#7，但按 32 字节块补齐（见官方 SDK 的
// crypto.ts），所以填充长度可以是 1–32，不能按 AES 的 16 字节块判断。
func decryptImage(ciphertext []byte, aesKey string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(aesKey)
	if err != nil {
		if key, err = base64.RawStdEncoding.DecodeString(aesKey); err != nil {
			return nil, fmt.Errorf("aeskey 不是合法的 Base64: %w", err)
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("aeskey 长度 %d，应为 32 字节", len(key))
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("密文长度无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(plain, ciphertext)

	const padBlock = 32
	n := int(plain[len(plain)-1])
	if n == 0 || n > padBlock || n > len(plain) {
		return nil, errors.New("PKCS#7 填充无效")
	}
	for _, b := range plain[len(plain)-n:] {
		if int(b) != n {
			return nil, errors.New("PKCS#7 填充无效")
		}
	}
	return plain[:len(plain)-n], nil
}
