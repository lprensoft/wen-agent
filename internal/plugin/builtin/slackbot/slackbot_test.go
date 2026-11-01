package slackbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"wen/internal/plugin"
	"wen/internal/session"
)

// ---------- 假 Slack：Socket Mode + Web API ----------

type sentMsg struct {
	channel string
	text    string
	mrkdwn  bool
}

type fakeSlack struct {
	t   *testing.T
	srv *httptest.Server

	sends chan sentMsg
	acks  chan string
	ready chan struct{}

	writeMu sync.Mutex

	mu            sync.Mutex
	conn          *websocket.Conn
	connections   int
	failNext      bool
	rateLimitOnce bool
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{
		t:     t,
		sends: make(chan sentMsg, 64),
		acks:  make(chan string, 64),
		ready: make(chan struct{}, 4),
	}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()

	mux.HandleFunc("/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xapp-test" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":  true,
			"url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws",
		})
	})

	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb-test" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		f.mu.Lock()
		rateLimited := f.rateLimitOnce
		if rateLimited {
			f.rateLimitOnce = false
		}
		fail := f.failNext
		f.failNext = false
		f.mu.Unlock()

		if rateLimited {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
			return
		}
		if fail {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "channel_not_found"})
			return
		}

		var body struct {
			Channel string `json:"channel"`
			Text    string `json:"text"`
			Mrkdwn  bool   `json:"mrkdwn"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.sends <- sentMsg{channel: body.Channel, text: body.Text, mrkdwn: body.Mrkdwn}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.2", "channel": body.Channel})
	})

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = conn
		f.connections++
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			if f.conn == conn {
				f.conn = nil
			}
			f.mu.Unlock()
			_ = conn.Close()
		}()
		f.writeMu.Lock()
		_ = conn.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		f.writeMu.Unlock()
		f.ready <- struct{}{}

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var ack map[string]string
			if json.Unmarshal(raw, &ack) == nil && ack["envelope_id"] != "" {
				f.acks <- ack["envelope_id"]
			}
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) pushFrame(frame any) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatal("Socket 尚未连接")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if err := conn.WriteJSON(frame); err != nil {
		f.t.Fatalf("写入 Socket 帧失败: %v", err)
	}
}

func (f *fakeSlack) pushEnvelope(id string, payload map[string]any) {
	f.pushFrame(map[string]any{
		"envelope_id": id, "type": "events_api", "payload": payload, "accepts_response_payload": false,
	})
}

func (f *fakeSlack) expectSend(t *testing.T) sentMsg {
	t.Helper()
	select {
	case m := <-f.sends:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("等待出站消息超时")
		return sentMsg{}
	}
}

func (f *fakeSlack) expectNoSend(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.sends:
		t.Fatalf("不应有出站消息，收到: %+v", m)
	case <-time.After(d):
	}
}

func (f *fakeSlack) expectAck(t *testing.T, envelopeID string) {
	t.Helper()
	select {
	case id := <-f.acks:
		if id != envelopeID {
			t.Fatalf("ack 的 envelope_id = %q，想要 %q", id, envelopeID)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("没有收到 %s 的 ack", envelopeID)
	}
}

func messagePayload(eventID, channel, user, text, channelType, subtype, botID string, files bool) map[string]any {
	event := map[string]any{
		"type": "message", "channel": channel, "user": user, "text": text,
		"ts": "1.1", "channel_type": channelType,
	}
	if subtype != "" {
		event["subtype"] = subtype
	}
	if botID != "" {
		event["bot_id"] = botID
	}
	if files {
		event["files"] = []map[string]any{{"id": "F1", "name": "a.png"}}
	}
	return map[string]any{"type": "event_callback", "event_id": eventID, "event": event}
}

// newInited 起一个连着假 Slack 的插件。默认关掉合并窗口，测试不必等 3 秒。
func newInited(t *testing.T, runTurn plugin.RunTurnFunc, whitelist string, extra map[string]any, tweaks ...func(*Plugin)) (*Plugin, *fakeSlack, *session.Store) {
	t.Helper()
	f := newFakeSlack(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := New()
	for _, tweak := range tweaks {
		tweak(p)
	}
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
		"app_token":        "xapp-test",
		"bot_token":        "xoxb-test",
		"api_base":         f.srv.URL,
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
		t.Fatal("等待 Socket Mode 连接超时")
	}
	return p, f, store
}

func noopTurn(context.Context, string, string) (string, error) { return "", nil }

// ---------- 全链路 ----------

func TestChatRoundTrip(t *testing.T) {
	runTurn := func(ctx context.Context, _, input string) (string, error) {
		if !plugin.IsInteractive(ctx) {
			t.Error("Slack 轮次应标记为真人交互")
		}
		return "收到：" + input, nil
	}
	_, f, _ := newInited(t, runTurn, "D1", nil)
	f.pushEnvelope("env-1", messagePayload("Ev1", "D1", "U1", "你好", "im", "", "", false))
	f.expectAck(t, "env-1")
	m := f.expectSend(t)
	if m.channel != "D1" || !m.mrkdwn || m.text != "收到：你好" {
		t.Fatalf("发送参数不符: %+v", m)
	}
}

func TestCallbacksKeepArrivalOrder(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return input, nil }
	_, f, _ := newInited(t, runTurn, "D1", nil)
	for i := 1; i <= 6; i++ {
		n := string(rune('0' + i))
		f.pushEnvelope("env-"+n, messagePayload("Ev"+n, "D1", "U1", n, "im", "", "", false))
	}
	for i := 1; i <= 6; i++ {
		want := string(rune('0' + i))
		if m := f.expectSend(t); m.text != want {
			t.Fatalf("第 %d 条回复顺序不对，得到 %q", i, m.text)
		}
	}
}

func TestWhitelistRejectsUnknownChannel(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "D1", nil)
	f.pushEnvelope("env-1", messagePayload("Ev1", "D2", "U2", "你好", "im", "", "", false))
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestIgnoresNonDirectMessages(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "D1", nil)
	f.pushEnvelope("env-1", messagePayload("Ev1", "C1", "U1", "频道消息", "channel", "", "", false))
	f.pushEnvelope("env-2", messagePayload("Ev2", "D1", "U1", "机器人", "im", "", "B1", false))
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestFileShareGetsNote(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "D1", nil)
	f.pushEnvelope("env-1", messagePayload("Ev1", "D1", "U1", "", "im", "file_share", "", true))
	if m := f.expectSend(t); !strings.Contains(m.text, "暂不支持该消息类型") {
		t.Fatalf("文件消息应回一句提示: %+v", m)
	}
}

func TestDuplicateEventDeduped(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return input, nil }
	_, f, _ := newInited(t, runTurn, "D1", nil)
	payload := messagePayload("Ev1", "D1", "U1", "只回一次", "im", "", "", false)
	f.pushEnvelope("env-1", payload)
	f.pushEnvelope("env-2", payload) // 同一个 event_id 重投
	if m := f.expectSend(t); m.text != "只回一次" {
		t.Fatalf("回复不符: %+v", m)
	}
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestBackgroundTurnPush(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "D1", nil)
	if err := p.core.Bind("D1", "sess-hb"); err != nil {
		t.Fatal(err)
	}
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
		SessionID: "sess-hb", Origin: "heartbeat", FinalText: "早上好，有新进展。",
	})
	m := f.expectSend(t)
	if m.channel != "D1" || !strings.Contains(m.text, "早上好") {
		t.Fatalf("后台轮次结果应推给绑定频道: %+v", m)
	}
}

func TestPushReportsSendFailure(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "D1", nil)
	f.mu.Lock()
	f.failNext = true
	f.mu.Unlock()
	if p.push(context.Background(), "D1", "hi") {
		t.Fatal("API 返回错误时 Push 应报告未送达")
	}
	if !p.push(context.Background(), "D1", "hi") {
		t.Fatal("API 正常时 Push 应报告送达")
	}
}

func TestRateLimitedRetriesOnce(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "D1", nil)
	f.mu.Lock()
	f.rateLimitOnce = true
	f.mu.Unlock()
	if !p.push(context.Background(), "D1", "hi") {
		t.Fatal("429 后应等待并重试成功")
	}
	f.expectSend(t)
}

func TestLongMessageChunks(t *testing.T) {
	text := strings.Repeat("字", 5000)
	runTurn := func(context.Context, string, string) (string, error) { return text, nil }
	_, f, _ := newInited(t, runTurn, "D1", nil)
	f.pushEnvelope("env-1", messagePayload("Ev1", "D1", "U1", "讲个长故事", "im", "", "", false))

	first := f.expectSend(t)
	total := chunkTotal(first.text)
	var body strings.Builder
	collect := func(m sentMsg) {
		if len([]rune(m.text)) > chunkLimit+16 {
			t.Fatalf("单条超限: %d rune", len([]rune(m.text)))
		}
		if i := strings.Index(m.text, ") "); i >= 0 {
			body.WriteString(m.text[i+2:])
		} else {
			body.WriteString(m.text)
		}
	}
	collect(first)
	for i := 2; i <= total; i++ {
		collect(f.expectSend(t))
	}
	if total < 2 {
		t.Fatalf("5000 字应分成多段，实际 %d", total)
	}
	if body.String() != text {
		t.Fatal("分段后内容不一致")
	}
}

func chunkTotal(s string) int {
	if !strings.HasPrefix(s, "(") {
		return 1
	}
	parts := strings.SplitN(strings.TrimPrefix(s, "("), "/", 2)
	if len(parts) != 2 {
		return 1
	}
	n := 0
	for _, r := range strings.SplitN(parts[1], ")", 2)[0] {
		if r < '0' || r > '9' {
			return 1
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return 1
	}
	return n
}

func TestPlainFormatDisablesMarkdown(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) { return "**加粗** 与 `代码`", nil }
	_, f, _ := newInited(t, runTurn, "D1", map[string]any{"format": formatPlain})
	f.pushEnvelope("env-1", messagePayload("Ev1", "D1", "U1", "hi", "im", "", "", false))
	m := f.expectSend(t)
	if m.mrkdwn {
		t.Fatal("纯文本模式应关闭 mrkdwn")
	}
	if strings.Contains(m.text, "**") || !strings.Contains(m.text, "加粗") || !strings.Contains(m.text, "「代码」") {
		t.Fatalf("纯文本格式应剥掉 markdown: %q", m.text)
	}
}

func TestSlackMarkdownConversion(t *testing.T) {
	in := "## 标题\n**粗体** [链接](https://example.com) <@U1> `code`\n```\n**不动** <@U1>\n```"
	out := toSlackMarkdown(in)
	for _, want := range []string{"*标题*", "*粗体*", "<https://example.com|链接>", "&lt;@U1&gt;", "`code`", "**不动** <@U1>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("转换结果缺少 %q:\n%s", want, out)
		}
	}
}

func TestDisconnectReconnects(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return "收到：" + input, nil }
	_, f, _ := newInited(t, runTurn, "D1", nil)
	f.pushFrame(map[string]any{"type": "disconnect", "reason": "refresh_requested"})

	select {
	case <-f.ready:
	case <-time.After(6 * time.Second):
		t.Fatal("disconnect 后没有重连")
	}
	f.pushEnvelope("env-2", messagePayload("Ev2", "D1", "U1", "还在吗", "im", "", "", false))
	if m := f.expectSend(t); m.text != "收到：还在吗" {
		t.Fatalf("重连后应继续收消息: %+v", m)
	}
	f.mu.Lock()
	n := f.connections
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("应重连一次，连接数 = %d", n)
	}
}

func TestBuildProxyValidation(t *testing.T) {
	for _, bad := range []string{"https://127.0.0.1:1080", "socks5h://127.0.0.1:1080", "127.0.0.1:1080"} {
		if _, _, err := buildProxy("", bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
	if _, _, err := buildProxy("", "socks5://127.0.0.1:1080"); err != nil {
		t.Fatalf("socks5 代理应被接受: %v", err)
	}
}

func TestInitRequiresCredentials(t *testing.T) {
	p := New()
	err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"bot_token": "123"})
	if err == nil {
		t.Fatal("缺 App-Level Token 时应拒绝启用")
	}
}
