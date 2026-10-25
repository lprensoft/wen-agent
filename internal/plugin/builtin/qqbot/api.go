package qqbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var apiClient = &http.Client{Timeout: 30 * time.Second}

// apiDo 发起一次带鉴权的开放平台 API 请求，返回响应体。
func (p *Plugin) apiDo(ctx context.Context, method, path string, body any) ([]byte, error) {
	p.mu.Lock()
	base, tokens := p.apiBase, p.tokens
	p.mu.Unlock()

	token, err := tokens.Get(ctx)
	if err != nil {
		return nil, err
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
	req.Header.Set("Authorization", "QQBot "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API %s %s 返回 HTTP %d: %s", method, path, resp.StatusCode, truncate(string(raw), 300))
	}
	return raw, nil
}

// getGatewayURL 取事件网关的 WebSocket 地址。
func (p *Plugin) getGatewayURL(ctx context.Context) (string, error) {
	raw, err := p.apiDo(ctx, http.MethodGet, "/gateway", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.URL == "" {
		return "", fmt.Errorf("网关地址解析失败: %s", truncate(string(raw), 200))
	}
	return out.URL, nil
}

// sendMode 是一条消息以什么身份发出。
type sendMode struct {
	msgID  string // 非空为被动回复（附 msg_seq 防重）
	seq    int
	wakeup bool // 互动召回：不带 msg_id，30 天内分四个时段各能发一条
}

// sendC2C 发送一条单聊文本消息。msgID 为空且不是召回时是主动消息——官方对主动消息
// 有严格限制，失败由调用方容忍。
func (p *Plugin) sendC2C(ctx context.Context, openid, content string, m sendMode) error {
	return p.postC2C(ctx, openid, map[string]any{"content": content, "msg_type": 0}, m)
}

// sendC2CMarkdown 发送一条原生 markdown 单聊消息（msg_type=2）。
// 注意 markdown 与 message_reference 互斥，这里不带引用。
func (p *Plugin) sendC2CMarkdown(ctx context.Context, openid, content string, m sendMode) error {
	return p.postC2C(ctx, openid, map[string]any{"msg_type": 2, "markdown": map[string]any{"content": content}}, m)
}

// sendInputNotify 发「正在输入」（msg_type=6），最长 60 秒，对方收到正式消息时自动消失。
func (p *Plugin) sendInputNotify(ctx context.Context, openid string, m sendMode) error {
	return p.postC2C(ctx, openid, map[string]any{
		"msg_type":     6,
		"input_notify": map[string]any{"input_type": 1, "input_second": 60},
	}, m)
}

func (p *Plugin) postC2C(ctx context.Context, openid string, body map[string]any, m sendMode) error {
	switch {
	case m.msgID != "":
		body["msg_id"] = m.msgID
		body["msg_seq"] = m.seq
	case m.wakeup:
		body["is_wakeup"] = true // 与 msg_id 互斥
	}
	_, err := p.apiDo(ctx, http.MethodPost, "/v2/users/"+openid+"/messages", body)
	return err
}
