package qqbot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/llm"
)

// qqAttachment 是 C2C 事件里的附件元数据。图片的地址带签名，直接 GET 即可，
// 不需要也不应该附上机器人 Token。语音附件带平台转写 asr_refer_text，
// 官方不保证每条都有，缺失时按不支持处理。
type qqAttachment struct {
	ContentType  string `json:"content_type"`
	Filename     string `json:"filename"`
	Size         int64  `json:"size"`
	URL          string `json:"url"`
	AsrReferText string `json:"asr_refer_text"`
}

// isVoiceAttachment 判定附件是否为语音。content_type 为 "voice" 或 audio/* 时一定是
// 语音；其余情况下，仅当类型缺失或是不明确的 octet-stream 时才按语音录制扩展名兜底。
// 明确的 file / image / video 等类型优先于扩展名：官方 SDK 靠付费 STT 兜底，敢把
// recording.amr 一律当语音，但我们只做原生转写，误判会让普通文件既无转写也不下载。
// inboundVoice 与 inboundFiles 共用这一判定。
func isVoiceAttachment(a qqAttachment) bool {
	if a.ContentType == "voice" || strings.HasPrefix(a.ContentType, "audio/") {
		return true
	}
	if a.ContentType != "" && a.ContentType != "application/octet-stream" {
		return false
	}
	switch strings.ToLower(filepath.Ext(a.Filename)) {
	case ".amr", ".silk", ".slk", ".slac":
		return true
	}
	return false
}

// inboundVoice 提取语音附件的平台转写文本。只读事件里现成的 asr_refer_text，
// 不下载音频、不调模型，零额外成本；没有转写的语音返回空，由调用方回不支持提示。
func (p *Plugin) inboundVoice(attachments []qqAttachment) string {
	var parts []string
	for _, a := range attachments {
		if !isVoiceAttachment(a) {
			continue
		}
		if s := strings.TrimSpace(a.AsrReferText); s != "" {
			parts = append(parts, i18n.T(p.langOf(), "plugin.qq_bot.voice_prefix", "【语音】")+s)
		}
	}
	return strings.Join(parts, "\n")
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

// inboundFiles 下载非图片附件。语音走 inboundVoice 的平台转写，视频仍留到后续版本，
// 都不在这里当普通文件处理。
func (p *Plugin) inboundFiles(ctx context.Context, attachments []qqAttachment) ([]llm.FilePart, string) {
	var out []llm.FilePart
	total := 0
	tooMany := false
	for _, a := range attachments {
		if strings.HasPrefix(a.ContentType, "image/") ||
			isVoiceAttachment(a) ||
			strings.HasPrefix(a.ContentType, "video/") ||
			a.URL == "" {
			continue
		}
		if len(out) >= imbot.MaxFiles {
			tooMany = true
			continue
		}
		if a.Size > int64(imbot.MaxFileBytes) || total+int(a.Size) > imbot.MaxFileTotal {
			return out, i18n.T(p.langOf(), "plugin.qq_bot.msg.file_too_large",
				"文件超过大小上限，请压缩后再发。")
		}
		data, err := downloadAttachment(ctx, a.URL, int64(imbot.MaxFileBytes))
		if err != nil {
			i18n.Logf(p.langOf(), "log.qq.file_failed", "qq_bot: 下载文件失败: %v", err)
			return out, i18n.T(p.langOf(), "plugin.qq_bot.msg.file_failed", "文件下载失败，请重发。")
		}
		// 事件里的 size 可能缺失或偏小，下载后按实际字节再复核一次。
		if total+len(data) > imbot.MaxFileTotal {
			return out, i18n.T(p.langOf(), "plugin.qq_bot.msg.file_too_large",
				"文件超过大小上限，请压缩后再发。")
		}
		f, err := imbot.FileFromBytes(data, a.Filename, a.ContentType)
		if err != nil {
			i18n.Logf(p.langOf(), "log.qq.file_invalid", "qq_bot: 文件无效（已跳过）: %v", err)
			return out, i18n.T(p.langOf(), "plugin.qq_bot.msg.file_failed", "文件下载失败，请重发。")
		}
		out = append(out, f)
		total += len(data)
	}
	if tooMany {
		return out, i18n.T(p.langOf(), "plugin.qq_bot.msg.file_too_many",
			"一次最多处理 4 个文件，其余文件未发送给模型。")
	}
	return out, ""
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
