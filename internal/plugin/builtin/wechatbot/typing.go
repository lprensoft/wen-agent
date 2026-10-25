package wechatbot

import (
	"context"
	"encoding/json"

	"wen/internal/i18n"
)

// 「正在输入」状态是锦上添花：任何失败都静默降级，绝不影响消息链路。
// typing_ticket 按用户缓存，通过 getconfig 获取，失效（发送报错）时清缓存下次重取。

// setTyping 向微信发「正在输入 / 取消」状态。on=true 开始输入，false 结束。
func (p *Plugin) setTyping(ctx context.Context, userID, contextToken string, on bool) {
	p.mu.Lock()
	creds := p.creds
	ticket := p.typing[userID]
	p.mu.Unlock()
	if !creds.valid() {
		return
	}

	if ticket == "" {
		var err error
		ticket, err = p.fetchTypingTicket(ctx, creds, userID, contextToken)
		if err != nil || ticket == "" {
			return
		}
		p.mu.Lock()
		p.typing[userID] = ticket
		p.mu.Unlock()
	}

	status := 1 // typing
	if !on {
		status = 2 // cancel
	}
	body := map[string]any{
		"ilink_user_id": userID,
		"typing_ticket": ticket,
		"status":        status,
		"base_info":     newBaseInfo(),
	}
	raw, err := apiPost(ctx, creds.BaseURL, "/ilink/bot/sendtyping", body, creds.BotToken, apiTimeout)
	if err != nil {
		return
	}
	var resp struct {
		Ret int `json:"ret"`
	}
	if json.Unmarshal(raw, &resp) == nil && resp.Ret != 0 {
		// ticket 可能过期：清缓存，下次重取
		p.mu.Lock()
		delete(p.typing, userID)
		p.mu.Unlock()
	}
}

// fetchTypingTicket 通过 getconfig 获取该用户的 typing_ticket。
func (p *Plugin) fetchTypingTicket(ctx context.Context, creds credentials, userID, contextToken string) (string, error) {
	body := map[string]any{
		"ilink_user_id": userID,
		"context_token": contextToken,
		"base_info":     newBaseInfo(),
	}
	raw, err := apiPost(ctx, creds.BaseURL, "/ilink/bot/getconfig", body, creds.BotToken, apiTimeout)
	if err != nil {
		return "", err
	}
	var resp struct {
		Ret          int    `json:"ret"`
		TypingTicket string `json:"typing_ticket"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	if resp.Ret != 0 {
		i18n.Logf(p.langOf(), "log.wechat.typing_ticket_failed", "wechat_bot: 获取输入状态票据失败 ret=%d（忽略）", resp.Ret)
		return "", nil
	}
	return resp.TypingTicket, nil
}
