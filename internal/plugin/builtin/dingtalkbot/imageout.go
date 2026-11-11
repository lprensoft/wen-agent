package dingtalkbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"wen/internal/llm"
)

// sendImage 先上传图片拿 media_id，再用 sampleImageMsg 发出去。钉钉的
// photoURL 允许直接填 media_id，不需要公网地址。
func (p *Plugin) sendImage(ctx context.Context, userID, _ string, img llm.ImagePart) error {
	mediaID, err := p.uploadMedia(ctx, "image", img.FileName(), img.Data)
	if err != nil {
		return err
	}
	return p.sendRobot(ctx, userID, "sampleImageMsg", map[string]string{"photoURL": mediaID})
}

// sendFile 用 sampleFile 发一个普通文件。fileType 取扩展名，钉钉按它决定
// 客户端里的打开方式。
func (p *Plugin) sendFile(ctx context.Context, userID, _ string, f llm.FilePart) error {
	mediaID, err := p.uploadMedia(ctx, "file", f.FileName(), f.Data)
	if err != nil {
		return err
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(f.FileName())), ".")
	if ext == "" {
		ext = "file"
	}
	return p.sendRobot(ctx, userID, "sampleFile", map[string]string{
		"mediaId":  mediaID,
		"fileName": f.FileName(),
		"fileType": ext,
	})
}

func (p *Plugin) uploadMedia(ctx context.Context, kind, filename string, data []byte) (string, error) {
	p.mu.Lock()
	tokens, client, base := p.tokens, p.client, p.oapiBase
	p.mu.Unlock()
	if tokens == nil || client == nil || base == "" {
		return "", fmt.Errorf("dingtalk_bot: 尚未初始化")
	}

	for attempt := 0; attempt < 2; attempt++ {
		token, err := tokens.get(ctx)
		if err != nil {
			return "", err
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := mw.WriteField("type", kind); err != nil {
			return "", err
		}
		fw, err := mw.CreateFormFile("media", filename)
		if err != nil {
			return "", err
		}
		if _, err := fw.Write(data); err != nil {
			return "", err
		}
		if err := mw.Close(); err != nil {
			return "", err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			base+uploadPath+"?access_token="+url.QueryEscape(token), &buf)
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		var out struct {
			Errcode int    `json:"errcode"`
			Errmsg  string `json:"errmsg"`
			MediaID string `json:"media_id"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", fmt.Errorf("上传媒体响应无法解析（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 200))
		}
		if resp.StatusCode == http.StatusOK && out.Errcode == 0 {
			if out.MediaID == "" {
				return "", fmt.Errorf("上传媒体没有返回 media_id")
			}
			return out.MediaID, nil
		}
		if resp.StatusCode == http.StatusUnauthorized || isOAPITokenError(out.Errcode) {
			tokens.invalidate()
			continue
		}
		return "", fmt.Errorf("上传媒体失败: HTTP %d errcode=%d %s", resp.StatusCode, out.Errcode, out.Errmsg)
	}
	return "", fmt.Errorf("刷新 AccessToken 后上传仍然失败")
}

// isOAPITokenError 判定老版 OpenAPI 的 token 失效错误码。
func isOAPITokenError(code int) bool {
	switch code {
	case 40001, 40014, 42001:
		return true
	}
	return false
}
