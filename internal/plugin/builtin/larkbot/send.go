package larkbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"wen/internal/i18n"
	"wen/internal/llm"

	"wen/internal/mdtext"
)

const apiTimeout = 30 * time.Second

// send 把文本发到飞书 / Lark。
//
// 普通文本消息不渲染 markdown，直发会满屏星号，所以默认走卡片 JSON 2.0 的 markdown
// 组件，由客户端渲染标题、列表、引用、代码块、加粗与链接；配成纯文本时先转成可读纯文本。
// 分段在转换之前做，按原始 markdown 切能保住代码块围栏的完整性。
//
// replyTo（入站消息的 message_id）在这里用不上：飞书按 open_id 直接发即可，
// 不像微信那样必须回带凭据。保留参数是为了对齐骨架的 Sender 接口。
func (p *Plugin) send(ctx context.Context, openID, text, _ string) {
	p.mu.Lock()
	format := p.format
	p.mu.Unlock()

	chunks := mdtext.ChunkMarkdown(text, chunkLimit)
	total := len(chunks)
	for i, chunk := range chunks {
		if total > 1 {
			chunk = fmt.Sprintf("(%d/%d) %s", i+1, total, chunk)
		}
		msgType, content := buildPayload(p.langOf(), format, chunk)
		if err := p.sendMessage(ctx, openID, msgType, content); err != nil {
			if ctx.Err() != nil {
				return
			}
			i18n.Logf(p.langOf(), "log.lark.send_failed", "%s: 发送失败: %v", p.Name(), err)
		}
	}
}

// buildPayload 按格式产出 msg_type 与 content。
// content 是「JSON 序列化后的字符串」——这是飞书接口的形状，不是普通的嵌套对象。
func buildPayload(lang, format, text string) (string, string) {
	if format == formatPlain {
		raw, _ := json.Marshal(map[string]string{"text": mdtext.ToPlainText(lang, text)})
		return "text", string(raw)
	}
	// JSON 2.0：schema 必须显式声明，否则按 1.0 解析。1.0 的 lark_md 只认 markdown 的
	// 一个子集（没有标题、列表与代码块），config.wide_screen_mode 也早已废弃
	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": []any{
				map[string]any{"tag": "markdown", "content": text},
			},
		},
	}
	raw, _ := json.Marshal(card)
	return "interactive", string(raw)
}

// sendImage 先上传图片拿 image_key，再发一条 msg_type=image 的消息。
// 上传接口的上限是 10 MB，这里只发已校验过的 5 MB 以内图片。
func (p *Plugin) sendImage(ctx context.Context, openID, _ string, img llm.ImagePart) error {
	p.mu.Lock()
	base, tokens, client := p.apiBase, p.tokens, p.client
	p.mu.Unlock()
	if tokens == nil || client == nil {
		return fmt.Errorf("尚未完成初始化")
	}
	token, err := tokens.Get(ctx)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("image_type", "message"); err != nil {
		return err
	}
	fw, err := mw.CreateFormFile("image", img.FileName())
	if err != nil {
		return err
	}
	if _, err := fw.Write(img.Data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/open-apis/im/v1/images", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ImageKey string `json:"image_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("上传图片响应无法解析（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if env.Code != 0 {
		return fmt.Errorf("上传图片返回 code=%d: %s", env.Code, env.Msg)
	}
	if env.Data.ImageKey == "" {
		return fmt.Errorf("上传图片没有返回 image_key")
	}
	content, _ := json.Marshal(map[string]string{"image_key": env.Data.ImageKey})
	return p.sendMessage(ctx, openID, "image", string(content))
}

// sendMessage 发一条消息给指定 open_id。
func (p *Plugin) sendMessage(ctx context.Context, openID, msgType, content string) error {
	body := map[string]string{
		"receive_id": openID,
		"msg_type":   msgType,
		"content":    content,
	}
	_, err := p.apiPost(ctx, "/open-apis/im/v1/messages?receive_id_type=open_id", body)
	return err
}

// apiPost 发一次带鉴权的开放平台请求，并把业务错误码转成 error。
func (p *Plugin) apiPost(ctx context.Context, path string, body any) ([]byte, error) {
	p.mu.Lock()
	base, tokens, client := p.apiBase, p.tokens, p.client
	p.mu.Unlock()
	if tokens == nil || client == nil {
		return nil, fmt.Errorf("尚未完成初始化")
	}

	token, err := tokens.Get(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("%s 响应无法解析（HTTP %d）: %s", path, resp.StatusCode, truncate(string(out), 200))
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("%s 返回 code=%d: %s", path, env.Code, env.Msg)
	}
	return out, nil
}
