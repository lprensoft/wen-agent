package telegrambot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const (
	apiTimeout      = 30 * time.Second
	longPollTimeout = 50 * time.Second
)

// apiError 是 Telegram 返回的业务错误（HTTP 状态码也一并带上，便于日志排查）。
type apiError struct {
	Code        int
	Description string
	// RetryAfter 是限流（429）时官方给出的等待秒数，0 表示没给。
	RetryAfter int
}

func (e *apiError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("Telegram 报错 %d: %s（建议 %d 秒后重试）", e.Code, e.Description, e.RetryAfter)
	}
	return fmt.Sprintf("Telegram 报错 %d: %s", e.Code, e.Description)
}

// isParseError 判定「MarkdownV2 解析失败」。转义漏了一个字符就是这个错，
// 消息会整条发不出去，因此要能识别出来当场降级为纯文本重发。
func (e *apiError) isParseError() bool {
	return e.Code == 400 && strings.Contains(strings.ToLower(e.Description), "can't parse entities")
}

// isConflict 判定「已设置 webhook」。getUpdates 与 webhook 互斥，这时长轮询
// 永远收不到消息，必须让用户知道原因而不是干等。
func (e *apiError) isConflict() bool { return e.Code == 409 }

// apiCall 调一次 Bot API。out 为 nil 时丢弃 result。
// client 分开传：长轮询要用超时更长的那个。
func (p *Plugin) apiCall(ctx context.Context, client *http.Client, method string, params, out any) error {
	p.mu.Lock()
	base, token := p.apiBase, p.token
	p.mu.Unlock()
	if token == "" {
		return fmt.Errorf("尚未配置 Bot Token")
	}

	var rd io.Reader
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	url := base + "/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rd)
	if err != nil {
		return err
	}
	if params != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return decodeAPIResult(method, raw, resp.StatusCode, out)
}

// apiCallMultipart 以 multipart/form-data 调一次 Bot API（发送文件用）。
func (p *Plugin) apiCallMultipart(ctx context.Context, client *http.Client, method string, fields map[string]string, fileField, fileName string, fileData []byte, out any) error {
	p.mu.Lock()
	base, token := p.apiBase, p.token
	p.mu.Unlock()
	if token == "" {
		return fmt.Errorf("尚未配置 Bot Token")
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return err
		}
	}
	fw, err := mw.CreateFormFile(fileField, fileName)
	if err != nil {
		return err
	}
	if _, err := fw.Write(fileData); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/"+method, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return decodeAPIResult(method, raw, resp.StatusCode, out)
}

// decodeAPIResult 解析 Bot API 的统一信封。
func decodeAPIResult(method string, raw []byte, status int, out any) error {
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s 响应无法解析（HTTP %d）: %s", method, status, truncate(string(raw), 200))
	}
	if !env.OK {
		code := env.ErrorCode
		if code == 0 {
			code = status
		}
		return &apiError{Code: code, Description: env.Description, RetryAfter: env.Parameters.RetryAfter}
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("%s 结果无法解析: %w", method, err)
		}
	}
	return nil
}

// update 是 getUpdates 返回的一条更新。图片取 photo 的最大尺寸或 image/*
// 的 document；其余媒体暂不下载。
type update struct {
	UpdateID int64          `json:"update_id"`
	Message  *updateMessage `json:"message"`
}

type updateMessage struct {
	MessageID int64 `json:"message_id"`
	Date      int64 `json:"date"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"` // private / group / supergroup / channel
	} `json:"chat"`
	Text    string `json:"text"`
	Caption string `json:"caption"`
	Photo   []struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
	} `json:"photo"`
	Document *struct {
		FileID   string `json:"file_id"`
		FileName string `json:"file_name"`
		MimeType string `json:"mime_type"`
		FileSize int64  `json:"file_size"`
	} `json:"document"`
}

// downloadFile 通过 getFile 取 file_path，再下载文件内容；超过 limit 直接拒绝，
// 不把大文件读进内存。
func (p *Plugin) downloadFile(ctx context.Context, fileID string, limit int64) ([]byte, error) {
	var info struct {
		FilePath string `json:"file_path"`
	}
	if err := p.apiCall(ctx, p.client, "getFile", map[string]any{"file_id": fileID}, &info); err != nil {
		return nil, err
	}
	if info.FilePath == "" {
		return nil, fmt.Errorf("getFile 未返回 file_path")
	}
	p.mu.Lock()
	base, token, client := p.apiBase, p.token, p.client
	p.mu.Unlock()
	if token == "" {
		return nil, fmt.Errorf("尚未配置 Bot Token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/file/bot"+token+"/"+info.FilePath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载文件失败（HTTP %d）", resp.StatusCode)
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

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
