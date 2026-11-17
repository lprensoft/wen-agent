package slackbot

import (
	"context"
	"testing"
	"time"

	"wen/internal/imbot"
)

func TestReactionUsesMessageTimestamp(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "D1", nil)
	if err := p.react(context.Background(), "D1", "1700000000.000100", imbot.ReactCheer); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-f.reactions:
		if body["channel"] != "D1" || body["timestamp"] != "1700000000.000100" || body["name"] != "muscle" {
			t.Fatalf("请求异常: %+v", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到 reactions.add")
	}
}

func TestReactionRejectsUnknownName(t *testing.T) {
	p, _, _ := newInited(t, noopTurn, "D1", nil)
	if err := p.react(context.Background(), "D1", "1700000000.000100", "不存在的表情"); err == nil {
		t.Error("未知的表情应当报错")
	}
}

func TestReactionCoversVocabulary(t *testing.T) {
	for _, name := range imbot.ReactionVocabulary {
		if _, ok := reactionName[name]; !ok {
			t.Errorf("表情 %q 没有 Slack 映射", name)
		}
	}
}
