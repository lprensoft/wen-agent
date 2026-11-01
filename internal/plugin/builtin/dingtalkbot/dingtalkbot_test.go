package dingtalkbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"wen/internal/plugin"
	"wen/internal/session"
)

// ---------- 假钉钉：Stream 接入点 + WebSocket + OpenAPI ----------

type sentMsg struct {
	userID    string
	robotCode string
	msgKey    string
	title     string
	text      string
}

type ackMsg struct {
	code      int
	messageID string
	data      string
}

type fakeDingTalk struct {
	t   *testing.T
	srv *httptest.Server

	sends chan sentMsg
	acks  chan ackMsg
	ready chan struct{}

	writeMu sync.Mutex

	mu              sync.Mutex
	conn            *websocket.Conn
	connections     int
	tokenHits       int
	unauthorizedOne bool
	failInvalid     bool
	failFlow        bool
}

func newFakeDingTalk(t *testing.T) *fakeDingTalk {
	f := &fakeDingTalk{
		t:     t,
		sends: make(chan sentMsg, 64),
		acks:  make(chan ackMsg, 64),
		ready: make(chan struct{}, 4),
	}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1.0/gateway/connections/open", func(w http.ResponseWriter, r *http.Request) {
		var req streamEndpointRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ClientID != "KEY123" || req.ClientSecret != "SECRET123" {
			t.Errorf("Stream 凭证不符: %+v", req)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		found := false
		for _, s := range req.Subscriptions {
			if s.Topic == callbackTopic {
				found = true
			}
		}
		if !found {
			t.Errorf("没有订阅机器人消息回调: %+v", req.Subscriptions)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"endpoint": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws",
			"ticket":   "TICKET123",
		})
	})

	mux.HandleFunc("/v1.0/oauth2/accessToken", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenHits++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "token123",
			"expireIn":    7200,
		})
	})

	mux.HandleFunc("/v1.0/robot/oToMessages/batchSend", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		unauthorized := f.unauthorizedOne
		if unauthorized {
			f.unauthorizedOne = false
		}
		invalid, flow := f.failInvalid, f.failFlow
		f.mu.Unlock()
		if unauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"invalidParameter.token.invalid","message":"token"}`))
			return
		}

		var body struct {
			RobotCode string   `json:"robotCode"`
			UserIDs   []string `json:"userIds"`
			MsgKey    string   `json:"msgKey"`
			MsgParam  string   `json:"msgParam"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var param struct {
			Title   string `json:"title"`
			Text    string `json:"text"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal([]byte(body.MsgParam), &param)
		content := param.Text
		if body.MsgKey == "sampleText" {
			content = param.Content
		}
		userID := ""
		if len(body.UserIDs) > 0 {
			userID = body.UserIDs[0]
		}
		f.sends <- sentMsg{
			userID: userID, robotCode: body.RobotCode, msgKey: body.MsgKey,
			title: param.Title, text: content,
		}

		resp := map[string]any{"processQueryKey": "q1"}
		if invalid {
			resp["invalidStaffIdList"] = []string{userID}
		}
		if flow {
			resp["flowControlledStaffIdList"] = []string{userID}
		}
		_ = json.NewEncoder(w).Encode(resp)
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
		f.ready <- struct{}{}
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var ack streamAck
			if json.Unmarshal(raw, &ack) == nil {
				f.acks <- ackMsg{code: ack.Code, messageID: ack.Headers["messageId"], data: ack.Data}
			}
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDingTalk) pushFrame(frame any) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatal("Stream 尚未连接")
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if err := conn.WriteJSON(frame); err != nil {
		f.t.Fatalf("写入 Stream 帧失败: %v", err)
	}
}

func (f *fakeDingTalk) pushCallback(messageID string, data map[string]any) {
	raw, _ := json.Marshal(data)
	f.pushFrame(map[string]any{
		"specVersion": "1.0",
		"type":        "CALLBACK",
		"time":        time.Now().UnixMilli(),
		"headers": map[string]string{
			"topic": callbackTopic, "messageId": messageID, "contentType": "application/json",
		},
		"data": string(raw),
	})
}

func (f *fakeDingTalk) pushSystem(topic, messageID, data string) {
	f.pushFrame(map[string]any{
		"specVersion": "1.0",
		"type":        "SYSTEM",
		"time":        time.Now().UnixMilli(),
		"headers":     map[string]string{"topic": topic, "messageId": messageID},
		"data":        data,
	})
}

func (f *fakeDingTalk) expectSend(t *testing.T) sentMsg {
	t.Helper()
	select {
	case m := <-f.sends:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("等待出站消息超时")
		return sentMsg{}
	}
}

func (f *fakeDingTalk) expectNoSend(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.sends:
		t.Fatalf("不应有出站消息，收到: %+v", m)
	case <-time.After(d):
	}
}

func (f *fakeDingTalk) expectAck(t *testing.T, messageID string) {
	t.Helper()
	for {
		select {
		case a := <-f.acks:
			if a.messageID == messageID {
				if a.code != 200 {
					t.Fatalf("ack code = %d", a.code)
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("没有收到 %s 的 ack", messageID)
		}
	}
}

func botData(conversationType, userID, msgType string) map[string]any {
	return map[string]any{
		"conversationId":   "cid1",
		"conversationType": conversationType,
		"msgId":            "m-" + msgType,
		"senderNick":       "tester",
		"senderStaffId":    userID,
		"robotCode":        "ding123",
		"msgtype":          msgType,
	}
}

func textData(userID, content string) map[string]any {
	d := botData("1", userID, "text")
	d["text"] = map[string]any{"content": content}
	return d
}

// newInited 起一个连着假钉钉的插件。默认关掉合并窗口，测试不必等 3 秒。
func newInited(t *testing.T, runTurn plugin.RunTurnFunc, whitelist string, extra map[string]any, tweaks ...func(*Plugin)) (*Plugin, *fakeDingTalk, *session.Store) {
	t.Helper()
	f := newFakeDingTalk(t)
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
		"app_key":          "KEY123",
		"app_secret":       "SECRET123",
		"robot_code":       "ding123",
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
		t.Fatal("等待 Stream 连接超时")
	}
	return p, f, store
}

func noopTurn(context.Context, string, string) (string, error) { return "", nil }

// ---------- 全链路 ----------

func TestChatRoundTrip(t *testing.T) {
	runTurn := func(ctx context.Context, _, input string) (string, error) {
		if !plugin.IsInteractive(ctx) {
			t.Error("钉钉轮次应标记为真人交互")
		}
		return "收到：" + input, nil
	}
	_, f, _ := newInited(t, runTurn, "user1", nil)
	f.pushCallback("stream-1", textData("user1", "你好"))
	f.expectAck(t, "stream-1")
	m := f.expectSend(t)
	if m.userID != "user1" || m.robotCode != "ding123" || m.msgKey != "sampleMarkdown" {
		t.Fatalf("发送参数不符: %+v", m)
	}
	if m.text != "收到：你好" || m.title == "" {
		t.Fatalf("消息内容或标题不符: %+v", m)
	}
}

func TestCallbacksKeepArrivalOrder(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return input, nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	for i := 1; i <= 6; i++ {
		n := string(rune('0' + i))
		f.pushCallback("stream-"+n, textData("user1", n))
	}
	for i := 1; i <= 6; i++ {
		want := string(rune('0' + i))
		if m := f.expectSend(t); m.text != want {
			t.Fatalf("第 %d 条回复顺序不对，得到 %q", i, m.text)
		}
	}
}

func TestWhitelistRejectsUnknownUser(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "user1", nil)
	f.pushCallback("stream-1", textData("stranger", "你好"))
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestGroupMessagesIgnored(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "user1", nil)
	d := textData("user1", "@机器人 你好")
	d["conversationType"] = "2"
	f.pushCallback("stream-1", d)
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestMissingStaffIDIgnored(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "user1", nil)
	f.pushCallback("stream-1", textData("", "你好"))
	f.expectNoSend(t, 300*time.Millisecond)
}

func TestAudioUsesRecognition(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return input, nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	d := botData("1", "user1", "audio")
	d["content"] = map[string]any{"duration": 4000, "recognition": "今天天气不错"}
	f.pushCallback("stream-1", d)
	if m := f.expectSend(t); m.text != "【语音】今天天气不错" {
		t.Fatalf("语音应使用 recognition: %+v", m)
	}
}

func TestRichTextKeepsTextParts(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return "收到：" + input, nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	d := botData("1", "user1", "richText")
	d["content"] = map[string]any{"richText": []map[string]any{
		{"text": "这是文字"},
		{"downloadCode": "abc"},
	}}
	f.pushCallback("stream-1", d)
	if m := f.expectSend(t); m.text != "收到：这是文字" {
		t.Fatalf("富文本应只取文字: %+v", m)
	}
}

func TestUnsupportedMessageGetsNote(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "user1", nil)
	d := botData("1", "user1", "picture")
	d["content"] = map[string]any{"downloadCode": "abc"}
	f.pushCallback("stream-1", d)
	if m := f.expectSend(t); !strings.Contains(m.text, "暂不支持该消息类型") {
		t.Fatalf("图片消息应回一句提示: %+v", m)
	}
}

func TestDuplicateStreamFrameDeduped(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return input, nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	d := textData("user1", "只回一次")
	f.pushCallback("stream-1", d)
	f.pushCallback("stream-1", d) // 服务端重推同一个 messageId
	if m := f.expectSend(t); m.text != "只回一次" {
		t.Fatalf("回复不符: %+v", m)
	}
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
	if m.userID != "user1" || !strings.Contains(m.text, "早上好") {
		t.Fatalf("后台轮次结果应推给绑定用户: %+v", m)
	}
}

func TestPushReportsSendFailure(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "user1", nil)
	f.mu.Lock()
	f.failInvalid = true
	f.mu.Unlock()
	if p.push(context.Background(), "user1", "hi") {
		t.Fatal("userid 无效时 Push 应报告未送达")
	}
	f.mu.Lock()
	f.failInvalid = false
	f.failFlow = true
	f.mu.Unlock()
	if p.push(context.Background(), "user1", "hi") {
		t.Fatal("被限流时 Push 应报告未送达")
	}
	f.mu.Lock()
	f.failFlow = false
	f.mu.Unlock()
	if !p.push(context.Background(), "user1", "hi") {
		t.Fatal("平台正常时 Push 应报告送达")
	}
}

func TestTokenRefreshedOnUnauthorized(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "user1", nil)
	f.mu.Lock()
	f.unauthorizedOne = true
	f.mu.Unlock()
	if !p.push(context.Background(), "user1", "hi") {
		t.Fatal("401 后刷新 token 应重试成功")
	}
	f.mu.Lock()
	hits := f.tokenHits
	f.mu.Unlock()
	if hits != 2 {
		t.Fatalf("应取两次 token，实际 %d", hits)
	}
}

func TestLongMessageChunks(t *testing.T) {
	text := strings.Repeat("字", 2000) // 6000 字节
	runTurn := func(context.Context, string, string) (string, error) { return text, nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	f.pushCallback("stream-1", textData("user1", "讲个长故事"))

	first := f.expectSend(t)
	total := chunkTotal(first.text)
	var body strings.Builder
	collect := func(m sentMsg) {
		if len(m.text) > chunkByteLimit+32 || len([]rune(m.text)) > chunkRuneLimit+32 {
			t.Fatalf("单条超限: %d 字节 %d rune", len(m.text), len([]rune(m.text)))
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
		t.Fatalf("6000 字节应分成多段，实际 %d", total)
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
	n, err := strconv.Atoi(strings.SplitN(parts[1], ")", 2)[0])
	if err != nil || n <= 0 {
		return 1
	}
	return n
}

func TestPlainFormatUsesTextMessage(t *testing.T) {
	runTurn := func(context.Context, string, string) (string, error) { return "**加粗** 与 `代码`", nil }
	_, f, _ := newInited(t, runTurn, "user1", map[string]any{"format": formatPlain})
	f.pushCallback("stream-1", textData("user1", "hi"))
	m := f.expectSend(t)
	if m.msgKey != "sampleText" {
		t.Fatalf("纯文本应使用 sampleText，实际 %q", m.msgKey)
	}
	if strings.Contains(m.text, "**") || !strings.Contains(m.text, "加粗") || !strings.Contains(m.text, "「代码」") {
		t.Fatalf("纯文本格式应剥掉 markdown: %q", m.text)
	}
}

func TestSystemPingAcked(t *testing.T) {
	_, f, _ := newInited(t, noopTurn, "user1", nil)
	f.pushSystem(topicPing, "ping-1", `{"hello":"world"}`)
	for {
		select {
		case a := <-f.acks:
			if a.messageID == "ping-1" {
				if a.code != 200 || !strings.Contains(a.data, "hello") {
					t.Fatalf("ping ack 不符: %+v", a)
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("没有收到 ping ack")
		}
	}
}

func TestServerDisconnectReconnects(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) { return "收到：" + input, nil }
	_, f, _ := newInited(t, runTurn, "user1", nil)
	f.pushSystem(topicDisconnect, "disc-1", "")

	// 服务端的 disconnect 是正常连接管理，应重新获取接入点并重连
	select {
	case <-f.ready:
	case <-time.After(6 * time.Second):
		t.Fatal("收到 disconnect 后没有重连")
	}
	f.pushCallback("stream-2", textData("user1", "还在吗"))
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

func TestInitRequiresCredentials(t *testing.T) {
	p := New()
	err := p.Init(plugin.InitContext{StateDir: t.TempDir()},
		map[string]any{"app_key": "123", "app_secret": "123"})
	if err == nil {
		t.Fatal("缺 RobotCode 时应拒绝启用")
	}
}
