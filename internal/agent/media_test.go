package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// replyMediaPlugin 的 show_pic 走核心的出站媒体回调，模拟 media 插件的 send_image。
type replyMediaPlugin struct{}

func (replyMediaPlugin) Name() string                                  { return "reply_media_plugin" }
func (replyMediaPlugin) Description() string                           { return "测试出站媒体" }
func (replyMediaPlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (replyMediaPlugin) SystemPrompt() string                          { return "" }
func (replyMediaPlugin) Tools() []plugin.Tool                          { return []plugin.Tool{replyMediaTool{}} }

type replyMediaTool struct{}

func (replyMediaTool) Name() string        { return "show_pic" }
func (replyMediaTool) Description() string { return "测试发图" }
func (replyMediaTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (replyMediaTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	fn := plugin.ReplyMediaFrom(ctx)
	if fn == nil {
		return "", fmt.Errorf("本轮没有出站媒体回调")
	}
	return "已发送", fn(llm.ImagePart{MediaType: "image/png", Data: []byte("png-bytes")})
}

// 工具发出的图片挂在收尾的那条 assistant 消息上：界面看得到，字节不在 JSONL 里，
// 模型请求里也不会出现 assistant 图片（Anthropic 只接受用户消息里的图）。
func TestAssistantMediaRecordedButNotSentToModel(t *testing.T) {
	store, id := newTestSession(t)
	ob := &observerPlugin{}
	mp := &mockProvider{turns: []mockTurn{
		{toolCalls: []llm.ToolCall{{ID: "c1", Name: "show_pic", Arguments: []byte(`{}`)}}},
		{content: ""},
	}}
	ag := New(mp, newTestManager(t, replyMediaPlugin{}, ob), store, Options{Model: "m", MaxTurns: 5})

	var events []Event
	var sunk bool
	ag.SetTurnSink(func(string, plugin.TurnEndEvent) { sunk = true })
	ag.Run(context.Background(), id, "给我看看", func(ev Event) { events = append(events, ev) })

	_, history, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var ref llm.ImagePart
	found := false
	for _, m := range history {
		if m.Role == llm.RoleAssistant && len(m.Media) > 0 {
			found = true
			ref = m.Media[0]
			if len(ref.Data) != 0 {
				t.Fatal("图片字节不该落进 JSONL")
			}
		}
	}
	if !found || ref.Name == "" || ref.MediaType != "image/png" {
		t.Fatalf("assistant 消息没有带上图片引用: %+v", ref)
	}
	if got, err := store.ReadImage(id, ref.Name); err != nil || string(got) != "png-bytes" {
		t.Fatalf("落盘的图片 = %q, %v", got, err)
	}

	// 第二次请求里不该有任何 assistant 图片
	if len(mp.reqs) < 2 {
		t.Fatalf("请求数 = %d", len(mp.reqs))
	}
	for _, m := range mp.reqs[1].Messages {
		if len(m.Images) > 0 && m.Role != llm.RoleUser {
			t.Fatalf("模型请求里出现了 %s 的图片", m.Role)
		}
	}

	// 界面收到一条 media 事件
	var sawMedia bool
	for _, ev := range events {
		if ev.Type == EventMedia && len(ev.Media) == 1 && ev.Media[0].Name == ref.Name {
			sawMedia = true
		}
	}
	if !sawMedia {
		t.Fatal("没有收到 media 事件")
	}

	// 只发图、文字为空：仍算一轮有新内容，未读与通知照常
	if len(ob.evs) != 1 || !ob.evs[0].HasMedia {
		t.Fatalf("轮次结束事件应带 HasMedia: %+v", ob.evs)
	}
	if !sunk {
		t.Fatal("发了图片的轮次应推给界面，即使文字是空的")
	}
}

// 压缩时助手图片的引用要搬到摘要消息上，否则压缩一次照片就从会话里消失。
func TestSummaryCarriesAssistantMedia(t *testing.T) {
	store, id := newTestSession(t)
	ag := New(&mockProvider{}, newTestManager(t), store, Options{Model: "m"})
	name, err := store.PutImage(id, "image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	msgs := []session.StoredMessage{{
		Message: llm.Message{Role: llm.RoleAssistant, Content: "给你看"},
		Media:   []llm.ImagePart{{Name: name, MediaType: "image/png", Size: 9}},
		TS:      time.Now(),
	}}

	_, _, retained, _, media := ag.summaryMedia(id, "zh", msgs)
	if len(retained) != 0 {
		t.Fatalf("不该保留整条消息: %+v", retained)
	}
	if len(media) != 1 || media[0].Name != name {
		t.Fatalf("摘要没有带走图片引用: %+v", media)
	}
	if strings.Contains(media[0].Name, "/") {
		t.Fatalf("引用应是会话内的短名: %q", media[0].Name)
	}
}

// toolThenErrorProvider 第一次返回一次发图工具调用，第二次调用直接失败，
// 用来复现「图片已发出、随后模型调用失败、由转译收尾」那条路径。
type toolThenErrorProvider struct{ calls int }

func (p *toolThenErrorProvider) ChatStream(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.calls++
	if p.calls == 1 {
		ch := make(chan llm.StreamEvent, 1)
		ch <- llm.StreamEvent{Type: llm.EventToolCalls, ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "show_pic", Arguments: []byte(`{}`)},
		}}
		close(ch)
		return ch, nil
	}
	return nil, errors.New("boom")
}

// 失败转译成功时，本轮已经发出去的图片必须挂在转译出来的 assistant 消息上：
// 否则消息通道收到了图，会话里却没有引用，刷新网页后图片就消失了。
func TestFailureTranslationKeepsSentMedia(t *testing.T) {
	store, id := newTestSession(t)
	tr := &translatorPlugin{text: "（走神）", ok: true}
	ag := New(&toolThenErrorProvider{}, newTestManager(t, tr, replyMediaPlugin{}), store, Options{Model: "m", MaxTurns: 5})

	var mediaEvents int
	ag.Run(plugin.WithInteractive(context.Background()), id, "给我看看", func(ev Event) {
		if ev.Type == EventMedia {
			mediaEvents++
		}
	})

	_, history, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var media []llm.ImagePart
	translated := false
	for _, m := range history {
		if m.Role == llm.RoleAssistant && m.Content == "（走神）" {
			translated = true
			media = m.Media
		}
	}
	if !translated {
		t.Fatal("转译文本没有落进会话")
	}
	if len(media) != 1 || media[0].Name == "" {
		t.Fatalf("转译消息没有带上图片引用: %+v", media)
	}
	if _, err := store.ReadImage(id, media[0].Name); err != nil {
		t.Fatalf("图片引用读不回来: %v", err)
	}
	if mediaEvents != 1 {
		t.Fatalf("图片事件应只发一次，实际 %d 次", mediaEvents)
	}
}
