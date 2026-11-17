package reaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"wen/internal/imbot"
	"wen/internal/plugin"
)

func newTool(t *testing.T, lang string) plugin.Tool {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{Lang: lang}, nil); err != nil {
		t.Fatal(err)
	}
	return p.Tools()[0]
}

func TestReactionWithoutChannelReportsNoTarget(t *testing.T) {
	_, err := newTool(t, "zh").Execute(context.Background(), json.RawMessage(`{"reaction":"赞"}`))
	if err == nil || !strings.Contains(err.Error(), "消息通道") {
		t.Fatalf("没有通道时应当说明没有可回应的消息，得到 %v", err)
	}
}

func TestReactionAcceptsEnglishNameAndPassesCanonical(t *testing.T) {
	var got string
	ctx := plugin.WithReplyReaction(context.Background(), func(reaction string) error {
		got = reaction
		return nil
	})
	out, err := newTool(t, "en").Execute(ctx, json.RawMessage(`{"reaction":"Laugh"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != imbot.ReactLaugh {
		t.Fatalf("回调应当收到中文规范值 %q，得到 %q", imbot.ReactLaugh, got)
	}
	if !strings.Contains(out, "Laugh") {
		t.Fatalf("回执应当按英文说出表情名: %q", out)
	}
}

func TestReactionMapsDeclinedReasonsToTheModel(t *testing.T) {
	cases := map[error]string{
		imbot.ErrReactionUnsupported: "不支持",
		imbot.ErrNoReactionTarget:    "没有可以回应",
		imbot.ErrReactionAlreadySent: "已经回应过",
	}
	for cause, want := range cases {
		ctx := plugin.WithReplyReaction(context.Background(), func(string) error { return cause })
		_, err := newTool(t, "zh").Execute(ctx, json.RawMessage(`{"reaction":"赞"}`))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("原因 %v 应当译成含 %q 的说明，得到 %v", cause, want, err)
		}
	}
}

func TestReactionPlatformErrorIsNotLeaked(t *testing.T) {
	ctx := plugin.WithReplyReaction(context.Background(), func(string) error {
		return errors.New("platform said: 详细错误")
	})
	_, err := newTool(t, "zh").Execute(ctx, json.RawMessage(`{"reaction":"赞"}`))
	if err == nil || strings.Contains(err.Error(), "详细错误") {
		t.Fatalf("平台原文不应进入模型上下文: %v", err)
	}
}

func TestVocabularyMatchesChannelNames(t *testing.T) {
	got := vocabulary.ZHValues()
	if strings.Join(got, "|") != strings.Join(imbot.ReactionVocabulary, "|") {
		t.Fatalf("插件词表与通道词表不一致: %v / %v", got, imbot.ReactionVocabulary)
	}
}

func TestReactionRejectsUnknownName(t *testing.T) {
	ctx := plugin.WithReplyReaction(context.Background(), func(string) error { return nil })
	if _, err := newTool(t, "zh").Execute(ctx, json.RawMessage(`{"reaction":"吐槽"}`)); err == nil {
		t.Fatal("词表之外的表情应当报错")
	}
}
