package discordbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/gorilla/websocket"

	"wen/internal/plugin"
	"wen/internal/session"
)

// ---------- 假 Discord：Gateway + REST 二合一 ----------

type sentMessage struct {
	channelID string
	content   string
	replyTo   string
}

type fakeDiscord struct {
	t   *testing.T
	srv *httptest.Server

	sends  chan sentMessage
	typing chan string
	ready  chan struct{}

	mu            sync.Mutex
	conn          *websocket.Conn
	rateLimitOnce bool
	rateLimitHits int
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{
		t:      t,
		sends:  make(chan sentMessage, 32),
		typing: make(chan string, 32),
		ready:  make(chan struct{}, 4),
	}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 60000}})
		var frame gatewayFrame
		if _, raw, err := conn.ReadMessage(); err != nil || json.Unmarshal(raw, &frame) != nil {
			return
		}
		if frame.Op != opIdentify && frame.Op != opResume {
			t.Errorf("期待 Identify/Resume，收到 op=%d", frame.Op)
			return
		}
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()
		_ = conn.WriteJSON(map[string]any{
			"op": opDispatch, "t": "READY", "s": 1,
			"d": map[string]any{
				"session_id": "gw-sess",
				"user":       map[string]any{"username": "测试机器人"},
			},
		})
		f.ready <- struct{}{}
		for { // 消耗心跳直到断开
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("POST /api/v10/channels/{channel}/messages", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content          string `json:"content"`
			MessageReference *struct {
				MessageID string `json:"message_id"`
				ChannelID string `json:"channel_id"`
			} `json:"message_reference"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		limited := f.rateLimitOnce
		if limited {
			f.rateLimitOnce = false
			f.rateLimitHits++
		}
		f.mu.Unlock()
		if limited {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": "rate limited", "retry_after": 0.01,
			})
			return
		}
		replyTo := ""
		if body.MessageReference != nil {
			replyTo = body.MessageReference.MessageID
		}
		f.sends <- sentMessage{channelID: r.PathValue("channel"), content: body.Content, replyTo: replyTo}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "sent-1"})
	})
	mux.HandleFunc("POST /api/v10/channels/{channel}/typing", func(w http.ResponseWriter, r *http.Request) {
		f.typing <- r.PathValue("channel")
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) pushMessage(messageID, channelID, guildID, authorID, content string, bot bool) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatal("网关尚未握手")
	}
	d := map[string]any{
		"id":         messageID,
		"channel_id": channelID,
		"content":    content,
		"author":     map[string]any{"id": authorID, "bot": bot, "username": "tester"},
	}
	if guildID != "" {
		d["guild_id"] = guildID
	}
	if err := conn.WriteJSON(map[string]any{"op": opDispatch, "t": "MESSAGE_CREATE", "s": 2, "d": d}); err != nil {
		f.t.Fatalf("推送消息失败: %v", err)
	}
}

func (f *fakeDiscord) pushRaw(raw string) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatal("网关尚未握手")
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
		f.t.Fatalf("推送裸帧失败: %v", err)
	}
}

func (f *fakeDiscord) expectSend(t *testing.T) sentMessage {
	t.Helper()
	select {
	case m := <-f.sends:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("等待出站消息超时")
		return sentMessage{}
	}
}

func (f *fakeDiscord) expectNoSend(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.sends:
		t.Fatalf("不应有出站消息，收到: %+v", m)
	case <-time.After(d):
	}
}

func (f *fakeDiscord) expectTyping(t *testing.T) string {
	t.Helper()
	select {
	case ch := <-f.typing:
		return ch
	case <-time.After(2 * time.Second):
		t.Fatal("等待「正在输入」超时")
		return ""
	}
}

// newInited 起一个连着假 Discord 的插件。默认关掉合并窗口，测试不必等 3 秒。
func newInited(t *testing.T, runTurn plugin.RunTurnFunc, whitelist string, extra map[string]any) (*Plugin, *fakeDiscord, *session.Store) {
	t.Helper()
	f := newFakeDiscord(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := New()
	ictx := plugin.InitContext{
		StateDir: t.TempDir(),
		Sessions: store,
		RunTurn:  runTurn,
		NewSession: func() (string, error) {
			m, err := store.Create()
			return m.ID, err
		},
		Status: func(string) (plugin.StatusInfo, error) {
			return plugin.StatusInfo{
				Provider: "testprov", Model: "m1", Thinking: "off", ContextLength: 131072,
				HasSession: true, MessageCount: 2, MeasuredTokens: 1234, EstTokens: 999,
			}, nil
		},
	}
	cfg := map[string]any{
		"bot_token":        "TESTTOKEN",
		"api_base":         f.srv.URL + "/api/v10",
		"gateway_url":      "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws",
		"whitelist":        whitelist,
		"merge_window_sec": 0,
	}
	for k, v := range extra {
		cfg[k] = v
	}
	if err := p.Init(ictx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	select {
	case <-f.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("等待网关握手超时")
	}
	return p, f, store
}

// ---------- 全链路 ----------

func TestChatRoundTrip(t *testing.T) {
	runTurn := func(ctx context.Context, _, input string) (string, error) {
		if !plugin.IsInteractive(ctx) {
			t.Error("Discord 轮次应标记为真人交互")
		}
		return "收到：" + input, nil
	}
	_, f, _ := newInited(t, runTurn, "dm1", nil)
	f.pushMessage("m1", "dm1", "", "u1", "你好", false)

	m := f.expectSend(t)
	if m.channelID != "dm1" || m.content != "收到：你好" {
		t.Fatalf("回复不符: %+v", m)
	}
	if m.replyTo != "m1" {
		t.Fatalf("第一段应引用入站消息 m1: %+v", m)
	}
}

func TestWhitelistRejectsUnknownChannel(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "dm1", nil)
	f.pushMessage("m1", "dm-other", "", "u1", "你好", false)
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestIgnoresGuildAndBotMessages(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "dm1", nil)
	f.pushMessage("m1", "dm1", "guild-1", "u1", "服务器消息", false)
	f.pushMessage("m2", "dm1", "", "bot-1", "机器人自己的消息", true)
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestTypingIndicator(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) { return "好", nil }
	_, f, _ := newInited(t, runTurn, "dm1", nil)
	f.pushMessage("m1", "dm1", "", "u1", "你好", false)
	if ch := f.expectTyping(t); ch != "dm1" {
		t.Fatalf("正在输入应发到 dm1，得到 %q", ch)
	}
	f.expectSend(t)
}

func TestBackgroundTurnPush(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "dm1", nil)
	if err := p.core.Bind("dm1", "sess-hb"); err != nil {
		t.Fatal(err)
	}
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
		SessionID: "sess-hb", Origin: "heartbeat", FinalText: "早上好，有新进展。",
	})
	m := f.expectSend(t)
	if m.channelID != "dm1" || !strings.Contains(m.content, "早上好") {
		t.Fatalf("后台轮次结果应推给绑定频道: %+v", m)
	}
	if m.replyTo != "" {
		t.Fatalf("后台推送不应带回复引用: %+v", m)
	}
}

func TestRateLimitRetriesOnce(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) { return "好", nil }
	_, f, _ := newInited(t, runTurn, "dm1", nil)
	f.mu.Lock()
	f.rateLimitOnce = true
	f.mu.Unlock()
	f.pushMessage("m1", "dm1", "", "u1", "你好", false)
	f.expectSend(t)
	f.mu.Lock()
	hits := f.rateLimitHits
	f.mu.Unlock()
	if hits != 1 {
		t.Fatalf("应触发一次 429 并重试，实际 %d", hits)
	}
}

func TestLongMessageChunks(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) {
		return strings.Repeat("字", 4500), nil
	}
	_, f, _ := newInited(t, runTurn, "dm1", nil)
	f.pushMessage("m1", "dm1", "", "u1", "你好", false)

	var got []sentMessage
	for i := 0; i < 3; i++ {
		got = append(got, f.expectSend(t))
	}
	f.expectNoSend(t, 200*time.Millisecond)
	if len(got) != 3 {
		t.Fatalf("4500 字应切成 3 段，得到 %d", len(got))
	}
	if !strings.HasPrefix(got[0].content, "(1/3) ") || !strings.HasPrefix(got[1].content, "(2/3) ") {
		t.Fatalf("分段应带序号: %q / %q", got[0].content, got[1].content)
	}
	if got[0].replyTo != "m1" || got[1].replyTo != "" || got[2].replyTo != "" {
		t.Fatalf("只有第一段应带回复引用: %+v", got)
	}
	for _, m := range got {
		if n := len([]rune(m.content)); n > 2000 {
			t.Fatalf("单段超过 2000 字: %d", n)
		}
	}
}

func TestPlainFormatStripsMarkdown(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) {
		return "**加粗** 与 `代码`", nil
	}
	_, f, _ := newInited(t, runTurn, "dm1", map[string]any{"format": "plain"})
	f.pushMessage("m1", "dm1", "", "u1", "你好", false)
	m := f.expectSend(t)
	if strings.Contains(m.content, "**") || strings.Contains(m.content, "`") {
		t.Fatalf("纯文本模式应去掉 markdown 标记: %q", m.content)
	}
}

func TestEmojiHeavyMessageStaysWithinUTF16Limit(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) {
		return strings.Repeat("😀", 1500), nil
	}
	_, f, _ := newInited(t, runTurn, "dm1", nil)
	f.pushMessage("m1", "dm1", "", "u1", "你好", false)

	var got []sentMessage
	for i := 0; i < 2; i++ {
		got = append(got, f.expectSend(t))
	}
	f.expectNoSend(t, 200*time.Millisecond)
	for _, m := range got {
		if n := len(utf16.Encode([]rune(m.content))); n > 2000 {
			t.Fatalf("单段 UTF-16 长度 %d 超过 2000: %q", n, m.content)
		}
	}
}

func TestMalformedFrameDoesNotDropConnection(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) { return "好", nil }
	_, f, _ := newInited(t, runTurn, "dm1", nil)
	f.pushRaw("{not json")
	f.pushMessage("m1", "dm1", "", "u1", "你好", false)
	m := f.expectSend(t)
	if m.content != "好" {
		t.Fatalf("坏帧之后的正常消息应继续处理: %+v", m)
	}
}

func TestInitRequiresToken(t *testing.T) {
	p := New()
	p.startGateway = func(context.Context) {}
	err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, nil)
	if err == nil {
		t.Fatal("没有 Bot Token 时应拒绝启用")
	}
}

func TestBuildProxy(t *testing.T) {
	if u, tr, err := buildProxy("", ""); err != nil || u != nil || tr != nil {
		t.Errorf("留空应直连: %v %v %v", u, tr, err)
	}
	for _, ok := range []string{"http://127.0.0.1:7890", "https://127.0.0.1:7890", "socks5://127.0.0.1:1080"} {
		if u, tr, err := buildProxy("", ok); err != nil || u == nil || tr == nil {
			t.Errorf("%s 应被接受: %v %v %v", ok, u, tr, err)
		}
	}
	for _, bad := range []string{"127.0.0.1:1080", "ftp://x:1", "socks5://"} {
		if _, _, err := buildProxy("", bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func noopTurn(context.Context, string, string) (string, error) { return "", nil }
