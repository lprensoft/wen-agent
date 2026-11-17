package telegrambot

import (
	"context"
	"fmt"
	"strconv"

	"wen/internal/imbot"
)

// reactionEmoji 把词表映射到 Telegram 的表情回应白名单（setMessageReaction 只接受其中的 emoji）。
var reactionEmoji = map[string]string{
	imbot.ReactLike:      "👍",
	imbot.ReactHeart:     "❤",
	imbot.ReactLaugh:     "😁",
	imbot.ReactCry:       "😭",
	imbot.ReactSurprise:  "😱",
	imbot.ReactThinking:  "🤔",
	imbot.ReactCelebrate: "🎉",
	imbot.ReactCheer:     "🔥",
	imbot.ReactGotIt:     "👌",
}

// react 给一条消息加表情回应。Bot API 的 setMessageReaction 只替换 bot 自己的那一个
// 回应，因此同一条消息上重复调用只会更新它，不会堆叠。
func (p *Plugin) react(ctx context.Context, chatID, replyTo, reaction string) error {
	emoji, ok := reactionEmoji[reaction]
	if !ok {
		return fmt.Errorf("telegram_bot: 未知的表情回应 %q", reaction)
	}
	messageID, err := strconv.ParseInt(replyTo, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram_bot: 回复凭据不是消息 ID: %q", replyTo)
	}
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()

	params := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"reaction":   []map[string]string{{"type": "emoji", "emoji": emoji}},
	}
	return p.apiCall(ctx, client, "setMessageReaction", params, nil)
}
