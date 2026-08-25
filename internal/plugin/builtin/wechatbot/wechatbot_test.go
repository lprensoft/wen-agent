package wechatbot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wen/internal/plugin"
	"wen/internal/session"
)

// ---------- 假 iLink 平台：扫码 + 收信长轮询 + 发消息三合一 ----------

type sentWx struct {
	to           string
	text         string
	contextToken string
	msgType      int
	msgState     int
}

type fakeILink struct {
	t     *testing.T
	srv   *httptest.Server
	inbox chan wxMessage // 注入给 getupdates 的入站消息
	sends chan sentWx    // sendmessage 捕获

	qrStatus chan statusResponse // 扫码状态剧本，空时返回 wait
	qrCount  atomic.Int32        // get_bot_qrcode 被调用的次数

	expireOnce atomic.Bool // 下一次 getupdates 返回 errcode -14
	bufSeq     atomic.Int32
	lastReqBuf atomic.Value // 最近一次 getupdates 请求携带的游标
}

func newFakeILink(t *testing.T) *fakeILink {
	f := &fakeILink{
		t:     t,
		inbox: make(chan wxMessage, 32),
		sends: make(chan sentWx, 32),
	}
	f.qrStatus = make(chan statusResponse, 8)
	f.lastReqBuf.Store("")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ilink/bot/get_bot_qrcode", func(w http.ResponseWriter, r *http.Request) {
		n := f.qrCount.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"qrcode":             fmt.Sprintf("QR%d", n),
			"qrcode_img_content": fmt.Sprintf("https://ilink.test/qr/%d", n),
		})
	})
	mux.HandleFunc("GET /ilink/bot/get_qrcode_status", func(w http.ResponseWriter, r *http.Request) {
		select {
		case st := <-f.qrStatus:
			_ = json.NewEncoder(w).Encode(st)
		case <-time.After(100 * time.Millisecond):
			_ = json.NewEncoder(w).Encode(statusResponse{Status: "wait"})
		}
	})
	mux.HandleFunc("POST /ilink/bot/getupdates", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer TESTTOKEN" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("AuthorizationType") != "ilink_bot_token" || r.Header.Get("X-WECHAT-UIN") == "" {
			f.t.Error("getupdates 缺少 iLink 鉴权头")
		}
		var req struct {
			GetUpdatesBuf string `json:"get_updates_buf"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lastReqBuf.Store(req.GetUpdatesBuf)
		if f.expireOnce.CompareAndSwap(true, false) {
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": errcodeExpired, "errmsg": "session timeout"})
			return
		}
		select {
		case msg := <-f.inbox:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ret":             0,
				"msgs":            []wxMessage{msg},
				"get_updates_buf": fmt.Sprintf("buf-%d", f.bufSeq.Add(1)),
			})
		case <-time.After(200 * time.Millisecond): // 模拟长轮询窗口内无消息
			_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0, "get_updates_buf": req.GetUpdatesBuf})
		}
	})
	mux.HandleFunc("POST /ilink/bot/sendmessage", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer TESTTOKEN" {
			w.WriteHeader(401)
			return
		}
		var req struct {
			Msg wxMessage `json:"msg"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		text := ""
		if len(req.Msg.ItemList) > 0 && req.Msg.ItemList[0].TextItem != nil {
			text = req.Msg.ItemList[0].TextItem.Text
		}
		if req.Msg.ClientID == "" {
			f.t.Error("sendmessage 缺少 client_id")
		}
		f.sends <- sentWx{
			to: req.Msg.ToUserID, text: text, contextToken: req.Msg.ContextToken,
			msgType: req.Msg.MessageType, msgState: req.Msg.MessageState,
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	mux.HandleFunc("POST /ilink/bot/getconfig", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0, "typing_ticket": "TICKET"})
	})
	mux.HandleFunc("POST /ilink/bot/sendtyping", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

var wxMsgCounter atomic.Int64

// pushText 注入一条私聊文本消息，返回其去重键（message_id）。
func (f *fakeILink) pushText(from, text string) int64 {
	id := wxMsgCounter.Add(1)
	f.inbox <- wxMessage{
		MessageID: id, FromUserID: from, ToUserID: "bot@im.bot",
		MessageType: msgTypeUser, ContextToken: "CTX-" + from,
		ItemList: []messageItem{{Type: 1, TextItem: &textItem{Text: text}}},
	}
	return id
}

func (f *fakeILink) expectSend(t *testing.T) sentWx {
	t.Helper()
	select {
	case m := <-f.sends:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("等待出站消息超时")
		return sentWx{}
	}
}

func (f *fakeILink) expectNoSend(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.sends:
		t.Fatalf("不应有出站消息，收到: %+v", m)
	case <-time.After(d):
	}
}

const binderID = "binder@im.wechat"

// writeCreds 预写绑定凭证（跳过扫码流程直接进入已绑定态）。
func writeCreds(t *testing.T, stateDir, baseURL string) {
	t.Helper()
	if err := saveCredentials(stateDir, credentials{
		BotToken: "TESTTOKEN", BaseURL: baseURL, BotID: "bot-1", BinderUserID: binderID,
	}); err != nil {
		t.Fatal(err)
	}
}

func testICtx(t *testing.T, store *session.Store, sessDir string, runTurn plugin.RunTurnFunc, stateDir string) plugin.InitContext {
	t.Helper()
	return plugin.InitContext{
		StateDir:   stateDir,
		Sessions:   store,
		RunTurn:    runTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
		Status: func(sessionID string) (plugin.StatusInfo, error) {
			return plugin.StatusInfo{
				Version:  "v9.9.9",
				Provider: "testprov", Model: "m1", Thinking: "off", ContextLength: 131072,
				HasSession: sessionID != "", MessageCount: 2, MeasuredTokens: 1234, EstTokens: 999,
			}, nil
		},
	}
}

// newBound 起一个已绑定假平台的插件。
func newBound(t *testing.T, runTurn plugin.RunTurnFunc, whitelist string) (*Plugin, *fakeILink, *session.Store, string) {
	t.Helper()
	return newBoundCfg(t, runTurn, whitelist, nil)
}

// newBoundCfg 同 newBound，extra 覆盖默认配置项。
func newBoundCfg(t *testing.T, runTurn plugin.RunTurnFunc, whitelist string, extra map[string]any) (*Plugin, *fakeILink, *session.Store, string) {
	t.Helper()
	f := newFakeILink(t)
	sessDir, stateDir := t.TempDir(), t.TempDir()
	store, err := session.NewStore(sessDir)
	if err != nil {
		t.Fatal(err)
	}
	writeCreds(t, stateDir, f.srv.URL)

	p := New()
	p.pauseOnExpired = 100 * time.Millisecond // 测试不等真的一小时
	cfg := map[string]any{"api_base": f.srv.URL, "whitelist": whitelist}
	for k, v := range extra {
		cfg[k] = v
	}
	if err := p.Init(testICtx(t, store, sessDir, runTurn, stateDir), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return p, f, store, stateDir
}

func echoTurn(ctx context.Context, _ string, input string) (string, error) {
	return "收到：" + input, nil
}

// ---------- 扫码绑定 ----------

// 完整绑定剧本：wait → scaned → confirmed，凭证落盘、动作文案变化、绑定人放行。
func TestBindFlow(t *testing.T) {
	f := newFakeILink(t)
	sessDir, stateDir := t.TempDir(), t.TempDir()
	store, _ := session.NewStore(sessDir)

	p := New()
	if err := p.Init(testICtx(t, store, sessDir, echoTurn, stateDir), map[string]any{"api_base": f.srv.URL}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	if acts := p.Actions(); acts[0].Label != "扫码绑定微信" {
		t.Fatalf("未绑定时动作文案不符: %q", acts[0].Label)
	}
	if err := p.StartAction(context.Background(), "bind"); err != nil {
		t.Fatal(err)
	}

	// 等出二维码
	waitFor(t, func() bool {
		st, _ := p.ActionState("bind")
		return st.Status == plugin.ActionPending && len(st.Image) > 0
	}, "等待二维码生成")

	f.qrStatus <- statusResponse{Status: "scaned"}
	f.qrStatus <- statusResponse{
		Status: "confirmed", BotToken: "TESTTOKEN", IlinkBotID: "bot-1",
		BaseURL: f.srv.URL, IlinkUserID: binderID,
	}

	waitFor(t, func() bool {
		st, _ := p.ActionState("bind")
		return st.Status == plugin.ActionDone
	}, "等待绑定完成")

	creds, err := loadCredentials(stateDir)
	if err != nil || !creds.valid() || creds.BinderUserID != binderID {
		t.Fatalf("凭证未正确落盘: %+v err=%v", creds, err)
	}
	// 「已绑定」是运行时事实，落在 Detail 里；按钮文案是固定的一句，它要能进语言包
	if acts := p.Actions(); !strings.Contains(acts[0].Detail, "bound") {
		t.Fatalf("绑定后该在 Detail 里说明: %+v", acts[0])
	}

	// 绑定成功后轮询已启动，绑定人无需配置白名单即可对话
	f.pushText(binderID, "你好")
	m := f.expectSend(t)
	if m.text != "收到：你好" || m.to != binderID {
		t.Fatalf("绑定人对话不符: %+v", m)
	}
}

// 二维码过期自动刷新，超过 3 次放弃。
func TestBindQRRefresh(t *testing.T) {
	f := newFakeILink(t)
	sessDir, stateDir := t.TempDir(), t.TempDir()
	store, _ := session.NewStore(sessDir)

	p := New()
	if err := p.Init(testICtx(t, store, sessDir, echoTurn, stateDir), map[string]any{"api_base": f.srv.URL}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	if err := p.StartAction(context.Background(), "bind"); err != nil {
		t.Fatal(err)
	}
	for range 4 { // 3 次刷新 + 第 4 次放弃
		f.qrStatus <- statusResponse{Status: "expired"}
	}
	waitFor(t, func() bool {
		st, _ := p.ActionState("bind")
		return st.Status == plugin.ActionError
	}, "等待多次过期后报错")
	if n := f.qrCount.Load(); n != 4 { // 初始 1 + 刷新 3
		t.Fatalf("二维码应取 4 次，实际 %d", n)
	}
}

// ---------- 消息链路 ----------

// 消息进来 → 跑一轮对话（带交互标记）→ 回复回带 context_token、markdown 原样、FINISH。
func TestChatRoundTrip(t *testing.T) {
	runTurn := func(ctx context.Context, sid, input string) (string, error) {
		if !plugin.IsInteractive(ctx) {
			t.Error("微信轮次应标记为真人交互")
		}
		return "**加粗**回复：" + input, nil
	}
	_, f, _, _ := newBound(t, runTurn, "")

	f.pushText(binderID, "你好")
	m := f.expectSend(t)
	if m.to != binderID || m.contextToken != "CTX-"+binderID {
		t.Fatalf("回复应回带入站 context_token: %+v", m)
	}
	if m.msgType != msgTypeBot || m.msgState != stateFinish {
		t.Fatalf("消息类型/状态不符: %+v", m)
	}
	if m.text != "**加粗**回复：你好" {
		t.Fatalf("默认应原样发 markdown: %q", m.text)
	}
}

// 白名单：配置内放行、绑定人始终放行、陌生人拒绝。
func TestWhitelist(t *testing.T) {
	_, f, _, _ := newBound(t, echoTurn, "friend@im.wechat")

	f.pushText("stranger@im.wechat", "在吗")
	f.expectNoSend(t, 600*time.Millisecond)

	f.pushText("friend@im.wechat", "在吗")
	if m := f.expectSend(t); m.to != "friend@im.wechat" {
		t.Fatalf("白名单用户应放行: %+v", m)
	}
	f.pushText(binderID, "在吗")
	if m := f.expectSend(t); m.to != binderID {
		t.Fatalf("绑定人应始终放行: %+v", m)
	}
}

// 群聊消息忽略、重复消息去重、语音用转写文本。
func TestGroupDedupVoice(t *testing.T) {
	var turns atomic.Int32
	runTurn := func(_ context.Context, _ string, input string) (string, error) {
		turns.Add(1)
		return "OK:" + input, nil
	}
	_, f, _, _ := newBound(t, runTurn, "")

	// 群消息忽略
	f.inbox <- wxMessage{
		MessageID: wxMsgCounter.Add(1), FromUserID: binderID, GroupID: "g1",
		MessageType: msgTypeUser, ContextToken: "CTX",
		ItemList: []messageItem{{Type: 1, TextItem: &textItem{Text: "群里喊你"}}},
	}
	f.expectNoSend(t, 500*time.Millisecond)

	// 同一 message_id 重复投递只处理一次
	dup := wxMessage{
		MessageID: wxMsgCounter.Add(1), FromUserID: binderID,
		MessageType: msgTypeUser, ContextToken: "CTX-dup",
		ItemList: []messageItem{{Type: 1, TextItem: &textItem{Text: "重复"}}},
	}
	f.inbox <- dup
	f.expectSend(t)
	f.inbox <- dup
	f.expectNoSend(t, 500*time.Millisecond)

	// 语音带转写
	f.inbox <- wxMessage{
		MessageID: wxMsgCounter.Add(1), FromUserID: binderID,
		MessageType: msgTypeUser, ContextToken: "CTX-v",
		ItemList: []messageItem{{Type: 3, VoiceItem: &voiceItem{Text: "帮我查天气"}}},
	}
	if m := f.expectSend(t); !strings.Contains(m.text, "OK:【语音】帮我查天气") {
		t.Fatalf("语音应以转写文本入轮次: %q", m.text)
	}

	// 图片等媒体：提示不支持，不进轮次
	before := turns.Load()
	f.inbox <- wxMessage{
		MessageID: wxMsgCounter.Add(1), FromUserID: binderID,
		MessageType: msgTypeUser, ContextToken: "CTX-i",
		ItemList: []messageItem{{Type: 2}},
	}
	if m := f.expectSend(t); !strings.Contains(m.text, "暂不支持") {
		t.Fatalf("媒体消息应提示不支持: %q", m.text)
	}
	if turns.Load() != before {
		t.Fatal("媒体消息不应进入对话轮次")
	}
}

// ---------- 命令 ----------

func TestCommands(t *testing.T) {
	_, f, store, _ := newBound(t, echoTurn, "")

	f.pushText(binderID, "/help")
	if m := f.expectSend(t); !strings.Contains(m.text, "/new") || !strings.Contains(m.text, "/deny") {
		t.Fatalf("/help 输出不符: %q", m.text)
	}

	f.pushText(binderID, "/new")
	m := f.expectSend(t)
	if !strings.Contains(m.text, "已新建会话") {
		t.Fatalf("/new 输出不符: %q", m.text)
	}
	if metas, _ := store.List(); len(metas) == 0 {
		t.Fatal("/new 后应有会话")
	}

	f.pushText(binderID, "/status")
	m = f.expectSend(t)
	// 措辞由 internal/statustext 的测试盯着，这里只验证命令确实接到了那份渲染
	for _, want := range []string{"📊 Wen Agent v9.9.9", "testprov / m1 · 思考深度 off",
		"1,234 / 131,072 tokens（占用 0.9%）"} {
		if !strings.Contains(m.text, want) {
			t.Fatalf("/status 缺少 %q:\n%s", want, m.text)
		}
	}

	f.pushText(binderID, "/nonsense")
	if m := f.expectSend(t); !strings.Contains(m.text, "未知命令") {
		t.Fatalf("未知命令输出不符: %q", m.text)
	}
}

// /apply 在确认等待中直达（不经 worker 队列），批准后轮次继续。
func TestConfirmApply(t *testing.T) {
	turnDone := make(chan bool, 1)
	runTurn := func(ctx context.Context, _ string, _ string) (string, error) {
		c, ok := plugin.ConfirmerFrom(ctx)
		if !ok {
			t.Error("微信轮次应有确认通道")
			return "", nil
		}
		approved, err := c(ctx, plugin.ConfirmRequest{Title: "危险操作", Detail: "rm -rf /tmp/x"})
		if err != nil {
			return "", err
		}
		turnDone <- approved
		return "已执行", nil
	}
	_, f, _, _ := newBound(t, runTurn, "")

	f.pushText(binderID, "删掉临时目录")
	if m := f.expectSend(t); !strings.Contains(m.text, "需要你确认") || !strings.Contains(m.text, "/apply") {
		t.Fatalf("确认请求文案不符: %q", m.text)
	}
	f.pushText(binderID, "/apply")
	if m := f.expectSend(t); !strings.Contains(m.text, "已允许") {
		t.Fatalf("/apply 回执不符: %q", m.text)
	}
	select {
	case ok := <-turnDone:
		if !ok {
			t.Fatal("确认结果应为允许")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待轮次继续超时")
	}
	if m := f.expectSend(t); m.text != "已执行" {
		t.Fatalf("最终回复不符: %q", m.text)
	}
}

// 后台轮次（心跳等）落到微信绑定的会话时，用该用户最近入站的 context_token 推送；
// 没有 token（从未来过消息）只记日志不发；前台与自己发起的轮次不推。
func TestBackgroundTurnPush(t *testing.T) {
	p, f, _, stateDir := newBound(t, echoTurn, "")

	// 先让绑定人发一条消息：建立会话映射并记住 context_token
	f.pushText(binderID, "你好")
	f.expectSend(t)
	sid := p.core.BoundSession(binderID)
	if sid == "" {
		t.Fatal("对话后应有会话映射")
	}

	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
		SessionID: sid, Origin: "heartbeat", FinalText: "**心跳**问候",
	})
	m := f.expectSend(t)
	if m.to != binderID || m.contextToken != "CTX-"+binderID {
		t.Fatalf("推送应用最近入站的 context_token: %+v", m)
	}
	if m.text != "**心跳**问候" {
		t.Fatalf("推送应原样发 markdown: %q", m.text)
	}

	// token 已持久化：重启后仍可推送
	if raw, err := os.ReadFile(stateDir + "/tokens.json"); err != nil || !strings.Contains(string(raw), "CTX-"+binderID) {
		t.Fatalf("context_token 应持久化: %v %s", err, raw)
	}

	// 前台轮次、自己发起的轮次、无 token 的用户：都不推
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: sid, Origin: "", FinalText: "前台"})
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: sid, Origin: "wechat_bot", FinalText: "自己"})
	if err := p.core.Bind("mute@im.wechat", "sess-mute"); err != nil {
		t.Fatal(err)
	}
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "sess-mute", Origin: "heartbeat", FinalText: "无token"})
	f.expectNoSend(t, 500*time.Millisecond)
}

// 展示开关：默认不装通知回调；开启后思考与工具调用按序推送。
func TestShowProcess(t *testing.T) {
	// 默认关闭：轮次 ctx 里不应有通知回调
	sawNotes := false
	plain := func(ctx context.Context, _, _ string) (string, error) {
		sawNotes = plugin.TurnNotesFrom(ctx) != nil
		return "ok", nil
	}
	_, f, _, _ := newBound(t, plain, "")
	f.pushText(binderID, "你好")
	f.expectSend(t)
	if sawNotes {
		t.Fatal("默认配置下不应安装过程通知回调")
	}

	// 都开启：思考 → 工具 → 最终回复，三条按序；工具名保留行内代码标记（由微信渲染）
	notesTurn := func(ctx context.Context, _, _ string) (string, error) {
		if fn := plugin.TurnNotesFrom(ctx); fn != nil {
			fn(plugin.TurnNote{Kind: plugin.NoteThinking, Text: "让我想想"})
			fn(plugin.TurnNote{Kind: plugin.NoteToolCalls, Tools: []string{"exec_command"}})
		}
		return "最终回复", nil
	}
	_, f2, _, _ := newBoundCfg(t, notesTurn, "",
		map[string]any{"show_thinking": true, "show_tools": true})
	f2.pushText(binderID, "查一下")
	m := f2.expectSend(t)
	if !strings.Contains(m.text, "🧠 思考过程") || !strings.Contains(m.text, "让我想想") {
		t.Fatalf("第一条应是思考链: %q", m.text)
	}
	m = f2.expectSend(t)
	if !strings.Contains(m.text, "🔧 调用工具") || !strings.Contains(m.text, "`exec_command`") {
		t.Fatalf("第二条应是工具名列表: %q", m.text)
	}
	if m = f2.expectSend(t); !strings.Contains(m.text, "最终回复") {
		t.Fatalf("第三条应是最终回复: %q", m.text)
	}
}

// ---------- 轮询循环 ----------

// 会话过期（-14）暂停后自动恢复。
func TestExpiredPauseAndResume(t *testing.T) {
	_, f, _, _ := newBound(t, echoTurn, "")

	f.expireOnce.Store(true)
	time.Sleep(300 * time.Millisecond) // 让 -14 与暂停发生（pauseOnExpired=100ms）
	f.pushText(binderID, "醒了吗")
	if m := f.expectSend(t); m.text != "收到：醒了吗" {
		t.Fatalf("暂停后应恢复收信: %+v", m)
	}
}

// 游标持久化：处理过消息后落盘，重启后首个请求带上旧游标。
func TestCursorPersistence(t *testing.T) {
	f := newFakeILink(t)
	sessDir, stateDir := t.TempDir(), t.TempDir()
	store, _ := session.NewStore(sessDir)
	writeCreds(t, stateDir, f.srv.URL)

	p := New()
	p.pauseOnExpired = 100 * time.Millisecond
	ictx := testICtx(t, store, sessDir, echoTurn, stateDir)
	if err := p.Init(ictx, map[string]any{"api_base": f.srv.URL}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	f.pushText(binderID, "第一条")
	f.expectSend(t)
	waitFor(t, func() bool {
		raw, _ := os.ReadFile(bufPath(stateDir))
		return strings.HasPrefix(string(raw), "buf-")
	}, "等待游标落盘")
	raw, _ := os.ReadFile(bufPath(stateDir))
	saved := strings.TrimSpace(string(raw))

	// 重启：Stop + 重新 Init（同一 StateDir）
	p.Stop()
	p2 := New()
	p2.pauseOnExpired = 100 * time.Millisecond
	if err := p2.Init(ictx, map[string]any{"api_base": f.srv.URL}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p2.Stop)

	waitFor(t, func() bool {
		return f.lastReqBuf.Load() == saved
	}, "重启后首个请求应携带持久化游标")
}

// waitFor 轮询等待条件成立。
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("超时：" + what)
}

// ---------- 消息格式 ----------

// 默认按 markdown 原样发送，标记不被剥掉（渲染交给微信的 ClawBot 界面）。
func TestSendMarkdownByDefault(t *testing.T) {
	md := func(ctx context.Context, _, _ string) (string, error) {
		return "**加粗** 与\n\n> 引用", nil
	}
	_, f, _, _ := newBound(t, md, "user1")
	f.pushText("user1", "hi")
	m := f.expectSend(t)
	if m.text != "**加粗** 与\n\n> 引用" {
		t.Fatalf("markdown 模式应原样发送: %q", m.text)
	}
}

// 配置为纯文本时剥掉 markdown 标记。
func TestSendPlainWhenConfigured(t *testing.T) {
	md := func(ctx context.Context, _, _ string) (string, error) {
		return "**加粗** 与\n\n> 引用", nil
	}
	_, f, _, _ := newBoundCfg(t, md, "user1", map[string]any{"format": "plain"})
	f.pushText("user1", "hi")
	m := f.expectSend(t)
	if strings.Contains(m.text, "**") || !strings.Contains(m.text, "｜ 引用") {
		t.Fatalf("纯文本模式应剥掉标记并转换引用: %q", m.text)
	}
}
