package qqbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// tokenSource 管理 App Access Token：惰性获取、过期前提前刷新、并发只发一次请求。
// 请求头形式为 `Authorization: QQBot {token}`（旧的 Bot {appid}.{token} 已废弃）。
type tokenSource struct {
	mu        sync.Mutex
	appID     string
	secret    string
	tokenURL  string
	client    *http.Client
	token     string
	expiresAt time.Time
	fetchedAt time.Time
}

// refreshAhead 在过期前多久就开始换新 token。官方只在到期前 60 秒内才下发新 token
// （新旧并行有效 60 秒），更早去取拿回来的还是原来那个——从前提前 5 分钟，于是最后
// 那几分钟里每次调用都会白取一次，可能撞上频控。50 秒落在窗口之内，又留了余量。
const refreshAhead = 50 * time.Second

// minRefetch 是两次取 token 之间的最短间隔：万一取回来的还是快到期的旧 token
// （服务端行为未完全确认），也不至于每次调用都去敲一次门。
const minRefetch = 10 * time.Second

func newTokenSource(appID, secret string) *tokenSource {
	return &tokenSource{
		appID:    appID,
		secret:   secret,
		tokenURL: defTokenURL,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// Get 返回一个当前有效的 access token，必要时同步刷新。
func (t *tokenSource) Get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Until(t.expiresAt) > refreshAhead {
		return t.token, nil
	}
	if t.token != "" && time.Now().Before(t.expiresAt) && time.Since(t.fetchedAt) < minRefetch {
		return t.token, nil
	}
	body, _ := json.Marshal(map[string]string{"appId": t.appID, "clientSecret": t.secret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 access token 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		AccessToken string `json:"access_token"`
		// 官方接口这里返回的是字符串形式的数字
		ExpiresIn json.Number `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("获取 access token 失败（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	ttl, _ := out.ExpiresIn.Int64()
	if ttl <= 0 {
		ttl = 7200
	}
	t.token = out.AccessToken
	t.fetchedAt = time.Now()
	t.expiresAt = t.fetchedAt.Add(time.Duration(ttl) * time.Second)
	return t.token, nil
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
