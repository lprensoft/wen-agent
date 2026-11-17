package larkbot

import (
	"context"
	"testing"
	"time"

	"wen/internal/imbot"
)

func TestReactionPostsEmojiTypeOnInboundMessage(t *testing.T) {
	p, f := newInited(t, NewFeishu, noopTurn, "ou_me", nil)
	if err := p.react(context.Background(), "om_123", imbot.ReactLaugh); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-f.reactions:
		if c.MessageID != "om_123" || c.EmojiType != "LAUGH" {
			t.Fatalf("请求异常: %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到添加表情回应的请求")
	}
}

func TestReactionRejectsUnknownName(t *testing.T) {
	p, _ := newInited(t, NewFeishu, noopTurn, "ou_me", nil)
	if err := p.react(context.Background(), "om_123", "不存在的表情"); err == nil {
		t.Error("未知的表情应当报错")
	}
}

func TestReactionCoversVocabulary(t *testing.T) {
	for _, name := range imbot.ReactionVocabulary {
		if _, ok := reactionType[name]; !ok {
			t.Errorf("表情 %q 没有飞书映射", name)
		}
	}
}
