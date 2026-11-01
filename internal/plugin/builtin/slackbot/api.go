package slackbot

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

const postMessagePath = "/chat.postMessage"

const maxRetryAfter = 30 * time.Second

type slackAPIError struct {
	code string
}

func (e *slackAPIError) Error() string {
	return "Slack API 返回错误: " + e.code
}

// postMessage 发一条消息。429 按 Retry-After 等一次再重试；其余错误直接返回。
func (p *Plugin) postMessage(ctx context.Context, channel, text string, mrkdwn bool) error {
	p.mu.Lock()
	token, base, client := p.botToken, p.apiBase, p.client
	p.mu.Unlock()

	body := map[string]any{
		"channel":      channel,
		"text":         text,
		"mrkdwn":       mrkdwn,
		"unfurl_links": false,
		"unfurl_media": false,
	}
	for attempt := 0; attempt < 2; attempt++ {
		raw, status, retryAfter, err := postMessageOnce(ctx, client, base+postMessagePath, token, body)
		if err != nil {
			return err
		}
		var resp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &resp)
		if status == http.StatusTooManyRequests || resp.Error == "ratelimited" {
			if attempt == 0 {
				wait := retryAfter
				if wait <= 0 {
					wait = time.Second
				}
				if wait <= maxRetryAfter {
					sleepCtx(ctx, wait)
					if ctx.Err() != nil {
						return ctx.Err()
					}
					continue
				}
			}
			return &slackAPIError{code: "ratelimited"}
		}
		if status != http.StatusOK || !resp.OK {
			code := resp.Error
			if code == "" {
				code = fmt.Sprintf("HTTP %d: %s", status, truncate(string(raw), 200))
			}
			return &slackAPIError{code: code}
		}
		return nil
	}
	return &slackAPIError{code: "ratelimited"}
}

func postMessageOnce(ctx context.Context, client *http.Client, url, token string, body any) ([]byte, int, time.Duration, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, 0, err
	}
	var retryAfter time.Duration
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			retryAfter = time.Duration(n) * time.Second
		}
	}
	return raw, resp.StatusCode, retryAfter, nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
