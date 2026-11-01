package dingtalkbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	tokenPath = "/v1.0/oauth2/accessToken"
	sendPath  = "/v1.0/robot/oToMessages/batchSend"

	// tokenRefreshMargin 提前刷新，避免卡在过期边界上。
	tokenRefreshMargin = 5 * time.Minute
)

// tokenSource 缓存应用 AccessToken。钉钉建议缓存并自行刷新，过期时间 7200 秒；
// 并发发送共用同一把锁，避免同时打 token 接口。
type tokenSource struct {
	client    *http.Client
	base      string
	appKey    string
	appSecret string

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newTokenSource(client *http.Client, base, appKey, appSecret string) *tokenSource {
	return &tokenSource{client: client, base: base, appKey: appKey, appSecret: appSecret}
}

func (t *tokenSource) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Now().Before(t.expiresAt.Add(-tokenRefreshMargin)) {
		return t.token, nil
	}

	body := map[string]string{"appKey": t.appKey, "appSecret": t.appSecret}
	raw, status, err := postJSON(ctx, t.client, t.base+tokenPath, "", body)
	if err != nil {
		return "", err
	}
	var resp struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int64  `json:"expireIn"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("解析 AccessToken 响应失败: %w", err)
	}
	if status != http.StatusOK || resp.AccessToken == "" {
		return "", apiErrorFrom(status, raw)
	}
	ttl := time.Duration(resp.ExpireIn) * time.Second
	if ttl <= tokenRefreshMargin {
		ttl = time.Hour
	}
	t.token, t.expiresAt = resp.AccessToken, time.Now().Add(ttl)
	return t.token, nil
}

func (t *tokenSource) invalidate() {
	t.mu.Lock()
	t.token = ""
	t.expiresAt = time.Time{}
	t.mu.Unlock()
}

// sendMarkdown 给单个用户发一条 sampleMarkdown 消息。token 失效时刷新一次再重试。
func (p *Plugin) sendMarkdown(ctx context.Context, userID, title, text string) error {
	p.mu.Lock()
	tokens, client, base, robotCode := p.tokens, p.client, p.apiBase, p.robotCode
	p.mu.Unlock()

	param, err := json.Marshal(map[string]string{"title": title, "text": text})
	if err != nil {
		return err
	}
	body := map[string]any{
		"robotCode": robotCode,
		"userIds":   []string{userID},
		"msgKey":    "sampleMarkdown",
		"msgParam":  string(param),
	}

	for attempt := 0; attempt < 2; attempt++ {
		token, err := tokens.get(ctx)
		if err != nil {
			return err
		}
		raw, status, err := postJSON(ctx, client, base+sendPath, token, body)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized || isTokenError(raw) {
			tokens.invalidate()
			continue
		}
		if status != http.StatusOK {
			return apiErrorFrom(status, raw)
		}

		var resp struct {
			InvalidStaffIDList        []string `json:"invalidStaffIdList"`
			FlowControlledStaffIDList []string `json:"flowControlledStaffIdList"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &resp); err != nil {
				return fmt.Errorf("解析发送响应失败: %w", err)
			}
		}
		if contains(resp.InvalidStaffIDList, userID) {
			return fmt.Errorf("钉钉返回无效的 userid: %s", userID)
		}
		if contains(resp.FlowControlledStaffIDList, userID) {
			return fmt.Errorf("钉钉对该用户限流: %s", userID)
		}
		return nil
	}
	return fmt.Errorf("刷新 AccessToken 后发送仍然失败")
}

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("钉钉接口返回 HTTP %d: %s %s", e.status, e.code, e.message)
	}
	return fmt.Sprintf("钉钉接口返回 HTTP %d: %s", e.status, e.message)
}

func apiErrorFrom(status int, raw []byte) error {
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &body)
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		msg = truncate(strings.TrimSpace(string(raw)), 300)
	}
	return &apiError{status: status, code: body.Code, message: msg}
}

func isTokenError(raw []byte) bool {
	var body struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	switch body.Code {
	case "invalidParameter.token.invalid", "token.notExisted":
		return true
	}
	return false
}

func postJSON(ctx context.Context, client *http.Client, url, token string, body any) ([]byte, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("x-acs-dingtalk-access-token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
