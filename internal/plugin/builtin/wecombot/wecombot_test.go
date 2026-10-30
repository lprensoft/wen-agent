package wecombot

import (
	"context"
	"encoding/json"
	"fmt"
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

// ---------- 假企业微信长连接 ----------

type sentFrame struct {
	kind     string // reply / push
	reqID    string
	chatID   string
	chatType int
	content  string
	streamID string
	finish   bool
}

type fakeWeCom struct {
	t   *testing.T
	srv *httptest.Server

	sends chan sentFrame
	pings chan struct{}
	ready chan struct{}

	writeMu sync.Mutex

	mu         sync.Mutex
	conn       *websocket.Conn
	failSends  bool
	ignorePing bool
	holdAck    int // 接下来几条 respond 不回执（模拟回执丢失）
	onAuth     func()
	subscribes int
}

func newFakeWeCom(t *testing.T) *fakeWeCom {
	f := &fakeWeCom{
		t:     t,
		sends: make(chan sentFrame, 32),
		pings: make(chan struct{}, 16),
		ready: make(chan struct{}, 4),
	}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			if f.conn == conn {
				f.conn = nil
			}
			f.mu.Unlock()
			_ = conn.Close()
		}()

		var sub wsFrame
		if _, raw, err := conn.ReadMessage(); err != nil || json.Unmarshal(raw, &sub) != nil {
			return
		}
		if sub.Cmd != cmdSubscribe {
			t.Errorf("首帧应是 %s，收到 %q", cmdSubscribe, sub.Cmd)
			return
		}
		var creds struct {
			BotID  string `json:"bot_id"`
			Secret string `json:"secret"`
		}
		if err := json.Unmarshal(sub.Body, &creds); err != nil || creds.BotID != "BOTID123" || creds.Secret != "SECRET123" {
			t.Errorf("订阅凭证不符: %+v", creds)
			return
		}
		f.ack(sub.Headers.ReqID, 0)
		f.mu.Lock()
		f.subscribes++
		onAuth := f.onAuth
		f.mu.Unlock()
		if onAuth != nil {
			onAuth()
		}
		f.ready <- struct{}{}

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame wsFrame
			if json.Unmarshal(raw, &frame) != nil {
				continue
			}
			switch frame.Cmd {
			case cmdPing:
				select {
				case f.pings <- struct{}{}:
				default:
				}
				f.mu.Lock()
				ignore := f.ignorePing
				f.mu.Unlock()
				if !ignore {
					f.ack(frame.Headers.ReqID, 0)
				}
			case cmdRespond, cmdSend:
				var body struct {
					ChatID   string `json:"chatid"`
					ChatType int    `json:"chat_type"`
					Markdown struct {
						Content string `json:"content"`
					} `json:"markdown"`
					Stream struct {
						ID      string `json:"id"`
						Finish  bool   `json:"finish"`
						Content string `json:"content"`
					} `json:"stream"`
				}
				_ = json.Unmarshal(frame.Body, &body)
				kind, content := "reply", body.Stream.Content
				if frame.Cmd == cmdSend {
					kind, content = "push", body.Markdown.Content
				}
				f.sends <- sentFrame{
					kind: kind, reqID: frame.Headers.ReqID,
					chatID: body.ChatID, chatType: body.ChatType, content: content,
					streamID: body.Stream.ID, finish: body.Stream.Finish,
				}
				f.mu.Lock()
				fail := f.failSends
				hold := false
				if frame.Cmd == cmdRespond && f.holdAck > 0 {
					hold = true
					f.holdAck--
				}
				f.mu.Unlock()
				switch {
				case fail:
					f.ack(frame.Headers.ReqID, 40001)
				case !hold:
					f.ack(frame.Headers.ReqID, 0)
				}
			}
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWeCom) write(v any) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatal("长连接尚未建立")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if err := conn.WriteJSON(v); err != nil {
		f.t.Fatalf("写入帧失败: %v", err)
	}
}

func (f *fakeWeCom) ack(reqID string, errcode int) {
	f.write(map[string]any{
		"headers": map[string]any{"req_id": reqID},
		"errcode": errcode,
		"errmsg":  "ok",
	})
}

func (f *fakeWeCom) pushCallback(msgID, reqID, chatType, userID, msgType string, body map[string]any) {
	b := map[string]any{
		"msgid":    msgID,
		"aibotid":  "BOTID123",
		"chattype": chatType,
		"from":     map[string]any{"userid": userID},
		"msgtype":  msgType,
	}
	if body != nil {
		b[msgType] = body
	}
	f.write(map[string]any{
		"cmd":     cmdCallback,
		"headers": map[string]any{"req_id": reqID},
		"body":    b,
	})
}

func (f *fakeWeCom) pushRaw(raw string) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatal("长连接尚未建立")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
		f.t.Fatalf("推送裸帧失败: %v", err)
	}
}

func (f *fakeWeCom) expectSend(t *testing.T) sentFrame {
	t.Helper()
	select {
	case m := <-f.sends:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("等待出站消息超时")
		return sentFrame{}
	}
}

func (f *fakeWeCom) expectNoSend(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.sends:
		t.Fatalf("不应有出站消息，收到: %+v", m)
	case <-time.After(d):
	}
}

// newInited 起一个连着假企业微信的插件。默认关掉合并窗口，测试不必等 3 秒。
func newInited(t *testing.T, runTurn plugin.RunTurnFunc, whitelist string, extra map[string]any, tweaks ...func(*Plugin)) (*Plugin, *fakeWeCom, *session.Store) {
	t.Helper()
	return newInitedWith(t, runTurn, whitelist, extra, nil, tweaks...)
}

func newInitedWith(t *testing.T, runTurn plugin.RunTurnFunc, whitelist string, extra map[string]any, fakeSetup func(*fakeWeCom), tweaks ...func(*Plugin)) (*Plugin, *fakeWeCom, *session.Store) {
	t.Helper()
	f := newFakeWeCom(t)
	if fakeSetup != nil {
		fakeSetup(f)
	}
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
		"bot_id":           "BOTID123",
		"secret":           "SECRET123",
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
		t.Fatal("等待长连接认证超时")
	}
	// 假服务端发完订阅回执就通知就绪，客户端可能还没处理到那一帧；等它把连接
	// 记下来再发消息，否则会撞上「长连接尚未建立」。
	deadline := time.Now().Add(5 * time.Second)
	for p.connSnapshot() == nil {
		if time.Now().After(deadline) {
			t.Fatal("等待客户端完成认证超时")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return p, f, store
}

func noopTurn(context.Context, string, string) (string, error) { return "", nil }

// ---------- 全链路 ----------

func TestChatRoundTrip(t *testing.T) {
	runTurn := func(ctx context.Context, _, input string) (string, error) {
		if !plugin.IsInteractive(ctx) {
			t.Error("企业微信轮次应标记为真人交互")
		}
		return "收到：" + input, nil
	}
	_, f, _ := newInited(t, runTurn, "user1", nil)
	f.pushCallback("msg-1", "cb-1", "single", "user1", "text", map[string]any{"content": "你好"})
	m := f.expectSend(t)
	if m.kind != "reply" || m.reqID != "cb-1" || m.content != "收到：你好" {
		t.Fatalf("回复应透传回调 req_id: %+v", m)
	}
	if m.chatID != "" {
		t.Fatalf("被动回复不应带 chatid: %+v", m)
	}
	if m.streamID == "" || !m.finish {
		t.Fatalf("普通回复应是 finish=true 的 stream 消息: %+v", m)
	}
}

func TestVoiceUsesTranscription(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return input, nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	f.pushCallback("msg-1", "cb-1", "single", "user1", "voice", map[string]any{"content": "今天天气不错"})
	m := f.expectSend(t)
	if m.content != "【语音】今天天气不错" {
		t.Fatalf("语音应使用转写文本: %+v", m)
	}
}

func TestUnsupportedMessageGetsNote(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "user1", nil)
	f.pushCallback("msg-1", "cb-1", "single", "user1", "image", map[string]any{"url": "https://example.com/a.png"})
	m := f.expectSend(t)
	if !strings.Contains(m.content, "暂不支持该消息类型") {
		t.Fatalf("图片消息应回一句提示: %+v", m)
	}
}

func TestMixedKeepsTextPart(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return "收到：" + input, nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	f.pushCallback("msg-1", "cb-1", "single", "user1", "mixed", map[string]any{
		"msg_item": []map[string]any{
			{"msgtype": "text", "text": map[string]any{"content": "这是文字"}},
			{"msgtype": "image", "image": map[string]any{"url": "https://example.com/a.png"}},
		},
	})
	m := f.expectSend(t)
	if m.content != "收到：这是文字" {
		t.Fatalf("图文混排应只取文字: %+v", m)
	}
}

func TestWhitelistRejectsUnknownUser(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "user1", nil)
	f.pushCallback("msg-1", "cb-1", "single", "stranger", "text", map[string]any{"content": "你好"})
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestGroupMessagesIgnored(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "user1", nil)
	f.pushCallback("msg-1", "cb-1", "group", "user1", "text", map[string]any{"content": "@机器人 你好"})
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestAssistantCompatMessagesIgnored(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "user1", nil)
	f.write(map[string]any{
		"cmd":     cmdCallback,
		"headers": map[string]any{"req_id": "cb-1"},
		"body": map[string]any{
			"msgid": "msg-1", "chattype": "single",
			"from":        map[string]any{"userid": "user1"},
			"msgtype":     "text",
			"text":        map[string]any{"content": "来自智能助理"},
			"compat_info": map[string]any{"source": "ai_assistant"},
		},
	})
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestBackgroundTurnPush(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "user1", nil)
	if err := p.core.Bind("user1", "sess-hb"); err != nil {
		t.Fatal(err)
	}
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
		SessionID: "sess-hb", Origin: "heartbeat", FinalText: "早上好，有新进展。",
	})
	m := f.expectSend(t)
	if m.kind != "push" || m.chatID != "user1" || m.chatType != 1 || !strings.Contains(m.content, "早上好") {
		t.Fatalf("后台轮次应按单聊主动推送: %+v", m)
	}
}

func TestPushReportsSendFailure(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "user1", nil)
	f.mu.Lock()
	f.failSends = true
	f.mu.Unlock()
	if p.push(context.Background(), "user1", "hi") {
		t.Fatal("平台返回 errcode 时 Push 应报告未送达")
	}
	f.mu.Lock()
	f.failSends = false
	f.mu.Unlock()
	if !p.push(context.Background(), "user1", "hi") {
		t.Fatal("平台正常时 Push 应报告送达")
	}
}

func TestLongMessageChunksWithinByteLimit(t *testing.T) {
	text := strings.Repeat("字", 8000) // 24000 字节，超过 20480 上限
	runTurn := func(context.Context, string, string) (string, error) { return text, nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	f.pushCallback("msg-1", "cb-1", "single", "user1", "text", map[string]any{"content": "讲个长故事"})

	first := f.expectSend(t)
	second := f.expectSend(t)
	if first.kind != "reply" || second.kind != "reply" {
		t.Fatalf("超长回复的每一段都应走被动回复: %+v %+v", first, second)
	}
	if first.reqID != second.reqID || first.streamID == second.streamID {
		t.Fatalf("同一回调共用 req_id，但每段要有自己的 stream.id: %+v %+v", first, second)
	}
	var body strings.Builder
	for _, m := range []sentFrame{first, second} {
		if len(m.content) > maxContentBytes {
			t.Fatalf("单条超字节上限: %d", len(m.content))
		}
		if !strings.HasPrefix(m.content, "(") {
			t.Fatalf("多段消息应带序号前缀: %q", m.content[:10])
		}
		if i := strings.Index(m.content, ") "); i >= 0 {
			body.WriteString(m.content[i+2:])
		}
	}
	if body.String() != text {
		t.Fatal("分段后内容不一致")
	}
}

func TestPlainFormatStripsMarkdown(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) { return "**加粗** 与 `代码`", nil }
	_, f, _ := newInited(t, runTurn, "user1", map[string]any{"format": formatPlain})
	f.pushCallback("msg-1", "cb-1", "single", "user1", "text", map[string]any{"content": "hi"})
	m := f.expectSend(t)
	if strings.Contains(m.content, "**") || !strings.Contains(m.content, "加粗") || !strings.Contains(m.content, "「代码」") {
		t.Fatalf("纯文本格式应剥掉 markdown: %q", m.content)
	}
}

func TestHumanPaceUsesSeparateStreams(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) { return "第一段。\n\n第二段。", nil }
	_, f, _ := newInited(t, runTurn, "user1", map[string]any{"human_pace": true})
	f.pushCallback("msg-1", "cb-1", "single", "user1", "text", map[string]any{"content": "你说"})
	first := f.expectSend(t)
	second := f.expectSend(t)
	if first.content != "第一段。" || second.content != "第二段。" {
		t.Fatalf("像人一样发消息应拆成两条: %+v %+v", first, second)
	}
	if first.kind != "reply" || second.kind != "reply" || first.reqID != second.reqID {
		t.Fatalf("同一回调的多条回复应共用 req_id: %+v %+v", first, second)
	}
	if first.streamID == "" || first.streamID == second.streamID {
		t.Fatalf("每条回复要有自己的 stream.id: %+v %+v", first, second)
	}
}

func TestMalformedFrameDoesNotDropConnection(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) { return "好", nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	f.pushRaw("{not json")
	f.pushCallback("msg-1", "cb-1", "single", "user1", "text", map[string]any{"content": "还在吗"})
	if m := f.expectSend(t); m.content != "好" {
		t.Fatalf("坏帧之后连接应继续工作: %+v", m)
	}
}

func TestHeartbeatPingAndAck(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "user1", nil, func(p *Plugin) {
		p.heartbeatEvery = 30 * time.Millisecond
		p.heartbeatWait = 500 * time.Millisecond
	})
	select {
	case <-f.pings:
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到心跳 ping")
	}
	// 回执正常时连接应保持，且心跳持续
	time.Sleep(100 * time.Millisecond)
	select {
	case <-f.pings:
	case <-time.After(3 * time.Second):
		t.Fatal("心跳没有继续")
	}
}

func TestHeartbeatTimeoutClosesConnection(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "user1", nil, func(p *Plugin) {
		p.heartbeatEvery = 30 * time.Millisecond
		p.heartbeatWait = 30 * time.Millisecond
	})
	f.mu.Lock()
	f.ignorePing = true
	f.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.connSnapshot() == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("心跳等不到回执时应断开连接")
}

func TestServerKickStopsConnection(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "user1", nil)
	f.write(map[string]any{
		"cmd":     cmdEvent,
		"headers": map[string]any{"req_id": "evt-1"},
		"body":    map[string]any{"event": map[string]any{"eventtype": "disconnected_event"}},
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.connSnapshot() == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("被新连接顶替后应停止使用旧连接")
}

func TestCallbacksKeepArrivalOrder(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return input, nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	for i := 1; i <= 6; i++ {
		n := fmt.Sprint(i)
		f.pushCallback("msg-"+n, "cb-"+n, "single", "user1", "text", map[string]any{"content": n})
	}
	for i := 1; i <= 6; i++ {
		want := fmt.Sprint(i)
		if m := f.expectSend(t); m.content != want {
			t.Fatalf("第 %d 条回复顺序不对，得到 %q", i, m.content)
		}
	}
}

func TestCallbackRightAfterSubscribeIsDispatched(t *testing.T) {
	_, f, _ := newInitedWith(t, noopTurn, "user1", nil, func(f *fakeWeCom) {
		f.mu.Lock()
		f.onAuth = func() {
			// 紧跟着订阅成功回执到达：此时客户端可能还没把连接登记好
			f.pushCallback("msg-early", "cb-early", "single", "user1", "image",
				map[string]any{"url": "https://example.com/a.png"})
		}
		f.mu.Unlock()
	})
	m := f.expectSend(t)
	if m.kind != "reply" || m.reqID != "cb-early" || !strings.Contains(m.content, "暂不支持") {
		t.Fatalf("认证后立即到达的消息应正常回提示: %+v", m)
	}
}

func TestLateAckDoesNotCompleteNextSegment(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) { return "第一段。\n\n第二段。", nil }
	_, f, _ := newInited(t, runTurn, "user1", map[string]any{"human_pace": true}, func(p *Plugin) {
		p.ackWait = 50 * time.Millisecond
	})
	f.mu.Lock()
	f.holdAck = 1
	f.mu.Unlock()

	f.pushCallback("msg-1", "cb-1", "single", "user1", "text", map[string]any{"content": "你说"})
	if m := f.expectSend(t); m.kind != "reply" || m.content != "第一段。" {
		t.Fatalf("第一段应走回复: %+v", m)
	}
	// 等到回执超时，再补发第一段的迟到回执；此时第二段还没开始发
	time.Sleep(150 * time.Millisecond)
	f.ack("cb-1", 0)

	second := f.expectSend(t)
	if second.kind != "push" || second.content != "第二段。" {
		t.Fatalf("回执未确认的 req_id 不能复用，第二段应改走主动推送: %+v", second)
	}
	f.expectNoSend(t, 200*time.Millisecond)
}

func TestCanceledReplyIsolatesReqID(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "user1", nil, func(p *Plugin) {
		p.ackWait = 2 * time.Second // 让取消先于回执超时发生
	})
	f.mu.Lock()
	f.holdAck = 1
	f.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- p.send(ctx, "user1", "第一段", "cb-1") }()

	if m := f.expectSend(t); m.kind != "reply" || m.content != "第一段" {
		t.Fatalf("第一段应走回复: %+v", m)
	}
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("上下文取消后不应报告发送成功")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后发送没有返回")
	}

	// 迟到的成功回执属于已取消的那次请求，不能影响下一段
	f.ack("cb-1", 0)

	if !p.send(context.Background(), "user1", "第二段", "cb-1") {
		t.Fatal("后续段落应通过主动推送送达")
	}
	second := f.expectSend(t)
	if second.kind != "push" || second.content != "第二段" {
		t.Fatalf("取消未确认的 req_id 不能复用，第二段应改走主动推送: %+v", second)
	}
	f.expectNoSend(t, 150*time.Millisecond)
}

func TestInitRequiresCredentials(t *testing.T) {
	p := New()
	err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"bot_id": "123"})
	if err == nil {
		t.Fatal("缺 Secret 时应拒绝启用")
	}
}
