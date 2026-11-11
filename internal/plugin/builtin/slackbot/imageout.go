package slackbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"wen/internal/llm"
)

// sendImage 走 Slack 的外部上传三步发一张图片。
func (p *Plugin) sendImage(ctx context.Context, channelID, _ string, img llm.ImagePart) error {
	return p.sendAttachment(ctx, channelID, img.FileName(), img.Data)
}

// sendFile 走 Slack 的外部上传三步发一个普通文件。
func (p *Plugin) sendFile(ctx context.Context, channelID, _ string, f llm.FilePart) error {
	return p.sendAttachment(ctx, channelID, f.FileName(), f.Data)
}

// sendAttachment 走 Slack 的外部上传三步：申请上传地址 → 上传文件 → 完成并投递到
// DM。Bot Token 需要 files:write；文件地址是 Slack 返回的签名地址，不附 Bot Token。
func (p *Plugin) sendAttachment(ctx context.Context, channelID, name string, data []byte) error {
	p.mu.Lock()
	token, base, client := p.botToken, p.apiBase, p.client
	p.mu.Unlock()
	if token == "" || client == nil {
		return fmt.Errorf("slack_bot: 尚未初始化")
	}

	uploadURL, fileID, err := p.requestUploadURL(ctx, client, base, token, name, len(data))
	if err != nil {
		return err
	}
	if err := uploadFile(ctx, client, uploadURL, name, data); err != nil {
		return err
	}

	raw, status, _, err := postMessageOnce(ctx, client, base+"/files.completeUploadExternal", token, map[string]any{
		"files":      []map[string]string{{"id": fileID}},
		"channel_id": channelID,
	})
	if err != nil {
		return err
	}
	return slackOK(raw, status)
}

func (p *Plugin) requestUploadURL(ctx context.Context, client *http.Client, base, token, name string, size int) (string, string, error) {
	form := url.Values{
		"filename": {name},
		"length":   {strconv.Itoa(size)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/files.getUploadURLExternal", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := slackOK(raw, resp.StatusCode); err != nil {
		return "", "", err
	}
	var out struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("解析上传地址失败: %w", err)
	}
	if out.UploadURL == "" || out.FileID == "" {
		return "", "", fmt.Errorf("Slack 未返回上传地址或 file_id")
	}
	return out.UploadURL, out.FileID, nil
}

func uploadFile(ctx context.Context, client *http.Client, uploadURL, name string, data []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("上传文件返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

// slackOK 把 Slack 的 {ok,error} 信封转成错误。
func slackOK(raw []byte, status int) error {
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &resp)
	if status == http.StatusOK && resp.OK {
		return nil
	}
	code := resp.Error
	if code == "" {
		code = fmt.Sprintf("HTTP %d: %s", status, truncate(string(raw), 200))
	}
	return &slackAPIError{code: code}
}
