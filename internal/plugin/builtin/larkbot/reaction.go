package larkbot

import (
	"context"
	"fmt"

	"wen/internal/imbot"
)

// reactionType 把词表映射成飞书 / Lark 的 emoji_type（两个租户域取值相同）。
var reactionType = map[string]string{
	imbot.ReactLike:      "THUMBSUP",
	imbot.ReactHeart:     "LOVE",
	imbot.ReactLaugh:     "LAUGH",
	imbot.ReactCry:       "CRY",
	imbot.ReactSurprise:  "WOW",
	imbot.ReactThinking:  "THINKING",
	imbot.ReactCelebrate: "APPLAUSE",
	imbot.ReactCheer:     "MUSCLE",
	imbot.ReactGotIt:     "OK",
}

// react 给一条消息加表情回应。messageID 是入站事件里的 message_id。
func (p *Plugin) react(ctx context.Context, messageID, reaction string) error {
	emojiType, ok := reactionType[reaction]
	if !ok {
		return fmt.Errorf("%s: 未知的表情回应 %q", p.v.name, reaction)
	}
	body := map[string]any{"reaction_type": map[string]string{"emoji_type": emojiType}}
	_, err := p.apiPost(ctx, "/open-apis/im/v1/messages/"+messageID+"/reactions", body)
	return err
}
