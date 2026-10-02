package llm

import (
	"context"
	"fmt"
	"time"

	"wen/internal/i18n"
)

// Probe 用极小的一次真实请求验证 base_url / API Key / 模型 id 是否可用，
// 收到首个事件即视为连通。
//
// 设置页的「测试连接」与首次运行向导共用它：两处各写一份的话，判据迟早会分叉——
// 一边说连得上、另一边说连不上，用户无从判断该信哪个。
func Probe(ctx context.Context, lang string, provider Provider, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	events, err := provider.ChatStream(ctx, ChatRequest{
		Model:     model,
		Messages:  []Message{{Role: RoleUser, Content: "ping"}},
		MaxTokens: 16,
		Thinking:  "off",
	})
	if err != nil {
		return err
	}
	// 收到首个事件即视为连通，随后 cancel 关闭剩余的流
	ev, ok := <-events
	if !ok {
		return fmt.Errorf("%s", i18n.T(lang, "llm.err.empty_reply", "模型没有返回任何内容"))
	}
	if ev.Type == EventError {
		return ev.Err
	}
	return nil
}
