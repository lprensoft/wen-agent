package discordbot

import (
	"context"
	"testing"
	"time"

	"wen/internal/imbot"
)

// reactionCall 是假服务端收到的一次表情回应。
type reactionCall struct {
	Channel string
	Message string
	Emoji   string
}

func TestReactionPutsOwnReactionOnMessage(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "dm1", nil)
	if err := p.react(context.Background(), "dm1", "m9", imbot.ReactLike); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-f.reactions:
		if c.Channel != "dm1" || c.Message != "m9" || c.Emoji != "👍" {
			t.Fatalf("请求异常: %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到表情回应请求")
	}
}

func TestReactionRejectsUnknownName(t *testing.T) {
	p, _, _ := newInited(t, noopTurn, "dm1", nil)
	if err := p.react(context.Background(), "dm1", "m9", "不存在的表情"); err == nil {
		t.Error("未知的表情应当报错")
	}
}

func TestReactionCoversVocabulary(t *testing.T) {
	for _, name := range imbot.ReactionVocabulary {
		if _, ok := reactionEmoji[name]; !ok {
			t.Errorf("表情 %q 没有 Discord 映射", name)
		}
	}
}
