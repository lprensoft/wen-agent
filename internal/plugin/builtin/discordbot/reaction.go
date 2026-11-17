package discordbot

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"wen/internal/imbot"
)

// reactionEmoji 把词表映射成 Unicode emoji；Discord 直接接受 Unicode 表情。
var reactionEmoji = map[string]string{
	imbot.ReactLike:      "👍",
	imbot.ReactHeart:     "❤️",
	imbot.ReactLaugh:     "😄",
	imbot.ReactCry:       "😢",
	imbot.ReactSurprise:  "😮",
	imbot.ReactThinking:  "🤔",
	imbot.ReactCelebrate: "🎉",
	imbot.ReactCheer:     "💪",
	imbot.ReactGotIt:     "✅",
}

// react 给一条私聊消息加表情回应。
func (p *Plugin) react(ctx context.Context, channelID, messageID, reaction string) error {
	emoji, ok := reactionEmoji[reaction]
	if !ok {
		return fmt.Errorf("discord_bot: 未知的表情回应 %q", reaction)
	}
	path := "/channels/" + channelID + "/messages/" + messageID + "/reactions/" + url.PathEscape(emoji) + "/@me"
	_, err := p.apiDo(ctx, http.MethodPut, path, nil)
	return err
}
