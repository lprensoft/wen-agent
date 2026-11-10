package qqbot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/llm"
)

// qqAttachment 是 C2C 事件里的附件元数据。图片的地址带签名，直接 GET 即可，
// 不需要也不应该附上机器人 Token。
type qqAttachment struct {
	ContentType string `json:"content_type"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
}

func (p *Plugin) inboundImages(ctx context.Context, attachments []qqAttachment) ([]llm.ImagePart, string) {
	var out []llm.ImagePart
	total := 0
	tooMany := false
	for _, a := range attachments {
		if !strings.HasPrefix(a.ContentType, "image/") || a.URL == "" {
			continue
		}
		if len(out) >= imbot.MaxImages {
			tooMany = true
			continue
		}
		if a.Size > imbot.MaxImageBytes || total+int(a.Size) > imbot.MaxImageTotal {
			return out, i18n.T(p.langOf(), "plugin.qq_bot.msg.image_too_large",
				"图片超过大小上限，请压缩后再发。")
		}
		data, err := downloadAttachment(ctx, a.URL, imbot.MaxImageBytes)
		if err != nil {
			i18n.Logf(p.langOf(), "log.qq.image_failed", "qq_bot: 下载图片失败: %v", err)
			return out, i18n.T(p.langOf(), "plugin.qq_bot.msg.image_failed", "图片下载失败，请重发。")
		}
		img, err := imbot.ImageFromBytes(data)
		if err != nil {
			i18n.Logf(p.langOf(), "log.qq.image_invalid", "qq_bot: 图片无效（已跳过）: %v", err)
			return out, i18n.T(p.langOf(), "plugin.qq_bot.msg.image_failed", "图片下载失败，请重发。")
		}
		// 事件里的 size 可能缺失或偏小，下载后按实际字节再复核一次总量。
		if total+len(data) > imbot.MaxImageTotal {
			return out, i18n.T(p.langOf(), "plugin.qq_bot.msg.image_too_large",
				"图片超过大小上限，请压缩后再发。")
		}
		out = append(out, img)
		total += len(data)
	}
	if tooMany {
		return out, i18n.T(p.langOf(), "plugin.qq_bot.msg.image_too_many",
			"一次最多处理 4 张图片，其余图片未发送给模型。")
	}
	return out, ""
}

func downloadAttachment(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("文件超过 %d MB", limit>>20)
	}
	return data, nil
}
