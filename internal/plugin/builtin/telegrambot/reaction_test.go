package telegrambot

import (
	"context"
	"testing"
	"time"

	"wen/internal/imbot"
)

// reactionCall 是假服务端收到的一次 setMessageReaction。
type reactionCall struct {
	ChatID    string `json:"chat_id"`
	MessageID int64  `json:"message_id"`
	Reaction  []struct {
		Type  string `json:"type"`
		Emoji string `json:"emoji"`
	} `json:"reaction"`
}

func TestReactionSetsEmojiOnInboundMessage(t *testing.T) {
	p, f := newInited(t, noopTurn, "42", nil)
	if err := p.react(context.Background(), "42", "7", imbot.ReactLaugh); err != nil {
		t.Fatal(err)
	}
	var c reactionCall
	select {
	case c = <-f.reactions:
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到 setMessageReaction")
	}
	if c.ChatID != "42" || c.MessageID != 7 || len(c.Reaction) != 1 {
		t.Fatalf("请求异常: %+v", c)
	}
	if c.Reaction[0].Type != "emoji" || c.Reaction[0].Emoji != "😁" {
		t.Fatalf("表情异常: %+v", c.Reaction)
	}
}

func TestReactionRejectsUnknownAndBadTarget(t *testing.T) {
	p, _ := newInited(t, noopTurn, "42", nil)
	if err := p.react(context.Background(), "42", "7", "不存在的表情"); err == nil {
		t.Error("未知的表情应当报错")
	}
	if err := p.react(context.Background(), "42", "not-a-number", imbot.ReactLike); err == nil {
		t.Error("回复凭据不是消息 ID 时应当报错")
	}
}

func TestReactionCoversVocabulary(t *testing.T) {
	for _, name := range imbot.ReactionVocabulary {
		if _, ok := reactionEmoji[name]; !ok {
			t.Errorf("表情 %q 没有 Telegram 映射", name)
		}
	}
}
