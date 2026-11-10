package qqbot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"wen/internal/i18n"
	"wen/internal/llm"
)

// sendImage 先上传富媒体拿 file_info，再以 msg_type=7 发出去。能挂在入站 msg_id
// 上就占用一次被动回复额度；额度用完或没有凭据时退回主动消息。
func (p *Plugin) sendImage(ctx context.Context, openid, replyTo string, img llm.ImagePart) error {
	if img.MediaType == "image/gif" {
		return fmt.Errorf("%s", i18n.T(p.langOf(), "plugin.qq_bot.err.image_gif", "QQ 暂不支持发送 GIF 图片"))
	}
	fileInfo, err := p.uploadImage(ctx, openid, img)
	if err != nil {
		return err
	}

	p.mu.Lock()
	limiter := p.limiter
	p.mu.Unlock()
	mode := sendMode{}
	if replyTo != "" && limiter != nil {
		if ok, seq := limiter.next(replyTo); ok {
			mode.msgID, mode.seq = replyTo, seq
		}
	}
	return p.postC2C(ctx, openid, map[string]any{
		"msg_type": 7,
		"media":    map[string]any{"file_info": fileInfo},
	}, mode)
}

func (p *Plugin) uploadImage(ctx context.Context, openid string, img llm.ImagePart) (string, error) {
	raw, err := p.apiDo(ctx, http.MethodPost, "/v2/users/"+openid+"/files", map[string]any{
		"file_type":    1,
		"file_name":    img.FileName(),
		"file_data":    base64.StdEncoding.EncodeToString(img.Data),
		"srv_send_msg": false,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		FileInfo string `json:"file_info"`
		TTL      int64  `json:"ttl"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("上传图片响应无法解析: %s", truncate(string(raw), 200))
	}
	if out.FileInfo == "" {
		return "", fmt.Errorf("上传图片没有返回 file_info: %s", truncate(string(raw), 200))
	}
	return out.FileInfo, nil
}
