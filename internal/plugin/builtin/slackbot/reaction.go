package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"wen/internal/imbot"
)

const reactionsAddPath = "/reactions.add"

// reactionName 把词表映射成 Slack 的 shortcode（reactions.add 只接受名字，不接受 Unicode）。
var reactionName = map[string]string{
	imbot.ReactLike:      "thumbsup",
	imbot.ReactHeart:     "heart",
	imbot.ReactLaugh:     "smile",
	imbot.ReactCry:       "sob",
	imbot.ReactSurprise:  "astonished",
	imbot.ReactThinking:  "thinking_face",
	imbot.ReactCelebrate: "tada",
	imbot.ReactCheer:     "muscle",
	imbot.ReactGotIt:     "white_check_mark",
}

// react 给一条私聊消息加表情回应。timestamp 是入站消息的 ts，它就是 Slack 里消息的标识。
// already_reacted 视为成功：同一个表情已经在那里了，结果与预期一致。
func (p *Plugin) react(ctx context.Context, channelID, ts, reaction string) error {
	name, ok := reactionName[reaction]
	if !ok {
		return fmt.Errorf("slack_bot: 未知的表情回应 %q", reaction)
	}
	p.mu.Lock()
	token, base, client := p.botToken, p.apiBase, p.client
	p.mu.Unlock()

	body := map[string]any{"channel": channelID, "timestamp": ts, "name": name}
	raw, status, _, err := postMessageOnce(ctx, client, base+reactionsAddPath, token, body)
	if err != nil {
		return err
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &resp)
	if status == http.StatusOK && (resp.OK || resp.Error == "already_reacted") {
		return nil
	}
	code := resp.Error
	if code == "" {
		code = fmt.Sprintf("HTTP %d: %s", status, truncate(string(raw), 200))
	}
	return &slackAPIError{code: code}
}
