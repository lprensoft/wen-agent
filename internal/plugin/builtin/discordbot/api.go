package discordbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// apiError 是 Discord REST 返回的非 2xx。RetryAfter 非零表示 429，
// 调用方可以按平台给的秒数等一次再重试。
type apiError struct {
	Status     int
	Code       int
	Message    string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("Discord API HTTP %d（code=%d）: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("Discord API HTTP %d: %s", e.Status, e.Message)
}

// apiDo 发起一次带 Bot 鉴权的 REST 请求，返回响应体。
func (p *Plugin) apiDo(ctx context.Context, method, path string, body any) ([]byte, error) {
	p.mu.Lock()
	client, base, token := p.client, p.apiBase, p.token
	p.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("discord_bot: 尚未初始化")
	}

	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("User-Agent", "WenBot (https://github.com/lprensoft/wen-agent)")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	return nil, parseAPIError(resp, raw)
}

// parseAPIError 读 Discord 的错误体；429 的等待时间优先取 body 的 retry_after，
// 再退回 Retry-After 响应头。
func parseAPIError(resp *http.Response, raw []byte) error {
	var payload struct {
		Message    string  `json:"message"`
		Code       int     `json:"code"`
		RetryAfter float64 `json:"retry_after"`
	}
	_ = json.Unmarshal(raw, &payload)
	retry := time.Duration(payload.RetryAfter * float64(time.Second))
	if retry <= 0 {
		if v := resp.Header.Get("Retry-After"); v != "" {
			if sec, err := strconv.ParseFloat(v, 64); err == nil {
				retry = time.Duration(sec * float64(time.Second))
			}
		}
	}
	if payload.Message == "" {
		payload.Message = string(raw)
	}
	return &apiError{Status: resp.StatusCode, Code: payload.Code, Message: payload.Message, RetryAfter: retry}
}

// sendMessage 在指定频道发一条文本消息。replyTo 非空时带 Discord 的回复引用。
// allowed_mentions 关掉所有解析，避免模型输出里的 @everyone / @here 真的通知全服。
func (p *Plugin) sendMessage(ctx context.Context, channelID, content, replyTo string) error {
	body := map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	if replyTo != "" {
		body["message_reference"] = map[string]any{
			"message_id":         replyTo,
			"channel_id":         channelID,
			"fail_if_not_exists": false,
		}
	}
	_, err := p.apiDo(ctx, http.MethodPost, "/channels/"+channelID+"/messages", body)
	return err
}

// sendTyping 在指定频道显示「正在输入」。Discord 的输入状态约 10 秒自动消失，
// 不需要显式关闭。
func (p *Plugin) sendTyping(ctx context.Context, channelID string) error {
	_, err := p.apiDo(ctx, http.MethodPost, "/channels/"+channelID+"/typing", nil)
	return err
}
