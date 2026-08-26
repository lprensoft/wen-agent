package larkbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"wen/internal/mdtext"
)

const apiTimeout = 30 * time.Second

// send 把文本发到飞书 / Lark。
//
// 普通文本消息不渲染 markdown，直发会满屏星号，所以默认走富文本卡片（lark_md），
// 由客户端渲染加粗、链接、代码等；配成纯文本时先转成可读纯文本。
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
			log.Printf("%s: 发送失败: %v", p.Name(), err)
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
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"elements": []any{
			map[string]any{
				"tag": "div",
				// lark_md 支持加粗、斜体、删除线、链接、行内代码等常用子集
				"text": map[string]any{"tag": "lark_md", "content": text},
			},
		},
	}
	raw, _ := json.Marshal(card)
	return "interactive", string(raw)
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
