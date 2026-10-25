package qqbot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"wen/internal/imbot"
	"wen/internal/plugin"
	"wen/internal/session"
)

// ---------- 假 QQ 开放平台：鉴权 + 网关 + 发消息 API 三合一 ----------

type sentMsg struct {
	openid  string
	content string
	msgType int
	msgID   string
	seq     int
}

type fakeQQ struct {
	t      *testing.T
	srv    *httptest.Server
	sends  chan sentMsg
	typing chan sentMsg // msg_type=6 的「正在输入」单独收，不混进正文

	mu             sync.Mutex
	conn           *websocket.Conn
	seq            int64
	rejectMarkdown bool // 模拟平台「不允许发送原生 markdown」(40034012)
	rejectActive   bool // 模拟主动消息被拒（不带 msg_id 也不是召回的）
	wakeups        int  // 收到的互动召回条数

	ready chan struct{} // 收到 Identify 并回了 READY
}

func newFakeQQ(t *testing.T) *fakeQQ {
	f := &fakeQQ{t: t, sends: make(chan sentMsg, 32), typing: make(chan sentMsg, 32), ready: make(chan struct{}, 4)}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/getAppAccessToken", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "TESTTOKEN", "expires_in": "7200"})
	})
	mux.HandleFunc("GET /gateway", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "QQBot TESTTOKEN" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws"})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 60000}})
		// 期待 Identify（或 Resume）
		var frame wsFrame
		if _, raw, err := conn.ReadMessage(); err != nil || json.Unmarshal(raw, &frame) != nil {
			return
		}
		if frame.Op != opIdentify && frame.Op != opResume {
			f.t.Errorf("期待 Identify/Resume，收到 op=%d", frame.Op)
			return
		}
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()
		f.push("READY", map[string]any{"session_id": "gw-sess", "user": map[string]string{"username": "测试机器人"}})
		f.ready <- struct{}{}
		for { // 消耗心跳直到断开
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("POST /v2/users/{openid}/messages", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content  string `json:"content"`
			MsgType  int    `json:"msg_type"`
			Markdown *struct {
				Content string `json:"content"`
			} `json:"markdown"`
			MsgID    string `json:"msg_id"`
			MsgSeq   int    `json:"msg_seq"`
			IsWakeup bool   `json:"is_wakeup"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.MsgType == 6 {
			f.typing <- sentMsg{openid: r.PathValue("openid"), msgType: 6, msgID: body.MsgID, seq: body.MsgSeq}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "typing-1"})
			return
		}
		f.mu.Lock()
		reject := f.rejectMarkdown
		if body.IsWakeup {
			f.wakeups++
		}
		rejectActive := f.rejectActive && body.MsgID == "" && !body.IsWakeup
		f.mu.Unlock()
		if rejectActive {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "主动消息被拒", "code": 40034024})
			return
		}
		if body.MsgType == 2 && reject {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "不允许发送原生 markdown", "code": 40034012})
			return
		}
		content := body.Content
		if body.Markdown != nil {
			content = body.Markdown.Content
		}
		f.sends <- sentMsg{openid: r.PathValue("openid"), content: content, msgType: body.MsgType, msgID: body.MsgID, seq: body.MsgSeq}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "out-1"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// push 向已连接的客户端推一条事件。
func (f *fakeQQ) push(eventType string, d any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn == nil {
		f.t.Fatalf("推送 %s 时网关尚未连接", eventType)
	}
	f.seq++
	raw, _ := json.Marshal(d)
	_ = f.conn.WriteJSON(map[string]any{"op": opDispatch, "s": f.seq, "t": eventType, "d": json.RawMessage(raw)})
}

var msgCounter atomic.Int64

// pushC2C 推一条私聊消息。
func (f *fakeQQ) pushC2C(openid, content string) string {
	id := fmt.Sprintf("msg-%d", msgCounter.Add(1))
	f.push("C2C_MESSAGE_CREATE", map[string]any{
		"id": id, "content": content,
		"author": map[string]string{"user_openid": openid},
	})
	return id
}

// expectSend 等待一条发出的消息。
func (f *fakeQQ) expectSend(t *testing.T) sentMsg {
	t.Helper()
	select {
	case m := <-f.sends:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("等待出站消息超时")
		return sentMsg{}
	}
}

func (f *fakeQQ) expectNoSend(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.sends:
		t.Fatalf("不应有出站消息，收到: %+v", m)
	case <-time.After(d):
	}
}

// newInited 起一个连着假平台的插件。
func newInited(t *testing.T, runTurn plugin.RunTurnFunc, whitelist string) (*Plugin, *fakeQQ, *session.Store) {
	t.Helper()
	return newInitedCfg(t, runTurn, whitelist, nil)
}

// newInitedCfg 同 newInited，extra 覆盖默认配置项。
func newInitedCfg(t *testing.T, runTurn plugin.RunTurnFunc, whitelist string, extra map[string]any) (*Plugin, *fakeQQ, *session.Store) {
	t.Helper()
	f := newFakeQQ(t)
	sessDir := t.TempDir()
	store, err := session.NewStore(sessDir)
	if err != nil {
		t.Fatal(err)
	}
	p := New()
	p.tokenURLOverride = f.srv.URL + "/app/getAppAccessToken"
	ictx := plugin.InitContext{
		StateDir:   t.TempDir(),
		Sessions:   store,
		RunTurn:    runTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
		Status: func(sessionID string) (plugin.StatusInfo, error) {
			return plugin.StatusInfo{
				Provider: "testprov", Model: "m1", Thinking: "off", ContextLength: 131072,
				HasSession: sessionID != "", MessageCount: 2, MeasuredTokens: 1234, EstTokens: 999,
			}, nil
		},
	}
	cfg := map[string]any{
		"app_id": "123", "app_secret": "secret",
		"api_base": f.srv.URL, "whitelist": whitelist,
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

// 消息进来 → 跑一轮对话 → 被动回复带 msg_id 与 msg_seq。
func TestChatRoundTrip(t *testing.T) {
	runTurn := func(ctx context.Context, sid, input string) (string, error) {
		if !plugin.IsInteractive(ctx) {
			t.Error("QQ 轮次应标记为真人交互")
		}
		return "收到：" + input, nil
	}
	_, f, _ := newInited(t, runTurn, "user1")

	id := f.pushC2C("user1", "你好")
	m := f.expectSend(t)
	if m.openid != "user1" || m.content != "收到：你好" {
		t.Fatalf("回复不符: %+v", m)
	}
	if m.msgID != id || m.seq <= 0 {
		t.Fatalf("被动回复应带 msg_id=%s 与正的 seq: %+v", id, m)
	}
	if m.msgType != 2 {
		t.Fatalf("默认应发原生 markdown（msg_type=2），得到 %d", m.msgType)
	}
}

// 平台拒绝原生 markdown 时当场降级纯文本重发，且能力缓存生效（后续直接发纯文本）。
func TestMarkdownFallback(t *testing.T) {
	runTurn := func(_ context.Context, _, input string) (string, error) {
		return "**加粗** `代码` " + input, nil
	}
	p, f, _ := newInited(t, runTurn, "user1")
	f.mu.Lock()
	f.rejectMarkdown = true
	f.mu.Unlock()

	f.pushC2C("user1", "一")
	m := f.expectSend(t)
	if m.msgType != 0 {
		t.Fatalf("markdown 被拒后应降级纯文本重发: %+v", m)
	}
	if strings.Contains(m.content, "**") || strings.Contains(m.content, "`") {
		t.Fatalf("纯文本重发应去除 markdown 标记: %s", m.content)
	}
	if !strings.Contains(m.content, "加粗") || !strings.Contains(m.content, "「代码」") {
		t.Fatalf("纯文本转换不符: %s", m.content)
	}
	if p.markdownAllowed("user1") {
		t.Fatal("被拒后能力缓存应关闭该用户的 markdown")
	}

	// 第二条：直接走纯文本，不再撞 400
	f.pushC2C("user1", "二")
	if m := f.expectSend(t); m.msgType != 0 {
		t.Fatalf("能力缓存生效后应直接发纯文本: %+v", m)
	}
}

// 后台轮次（心跳等）落到 QQ 绑定的会话时，把最终文本以主动消息推给绑定用户；
// 前台轮次与本插件自己发起的轮次不推。
func TestBackgroundTurnPush(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "user1")
	if err := p.core.Bind("user1", "sess-hb"); err != nil {
		t.Fatal(err)
	}

	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
		SessionID: "sess-hb", Origin: "heartbeat", FinalText: "早上好，有新进展。",
	})
	m := f.expectSend(t)
	if m.openid != "user1" || !strings.Contains(m.content, "早上好") {
		t.Fatalf("后台轮次结果应推给绑定用户: %+v", m)
	}
	if m.msgID != "" {
		t.Fatalf("推送应是主动消息（无 msg_id）: %+v", m)
	}

	// 前台轮次、自己发起的轮次、未绑定会话：都不推
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "sess-hb", Origin: "", FinalText: "前台"})
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "sess-hb", Origin: "qq_bot", FinalText: "自己"})
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "sess-other", Origin: "heartbeat", FinalText: "无关"})
	f.expectNoSend(t, 500*time.Millisecond)
}

func noopTurn(context.Context, string, string) (string, error) { return "", nil }

// notesTurn 模拟核心在轮次中发出过程通知：一段思考、一批工具、最终文本。
func notesTurn(ctx context.Context, _, _ string) (string, error) {
	if fn := plugin.TurnNotesFrom(ctx); fn != nil {
		fn(plugin.TurnNote{Kind: plugin.NoteThinking, Text: "让我想想"})
		fn(plugin.TurnNote{Kind: plugin.NoteToolCalls, Tools: []string{"exec_command", "read_file"}})
	}
	return "最终回复", nil
}

// 展示开关：默认不装通知回调；开启后思考与工具调用按序推送、格式与 Web UI 对齐。
func TestShowProcess(t *testing.T) {
	// 默认关闭：轮次 ctx 里不应有通知回调
	sawNotes := false
	plain := func(ctx context.Context, _, _ string) (string, error) {
		sawNotes = plugin.TurnNotesFrom(ctx) != nil
		return "ok", nil
	}
	_, f, _ := newInited(t, plain, "user1")
	f.pushC2C("user1", "你好")
	f.expectSend(t)
	if sawNotes {
		t.Fatal("默认配置下不应安装过程通知回调")
	}

	// 都开启：思考 → 工具 → 最终回复，三条按序
	_, f2, _ := newInitedCfg(t, notesTurn, "user1",
		map[string]any{"show_thinking": true, "show_tools": true})
	f2.pushC2C("user1", "查一下")
	m := f2.expectSend(t)
	if !strings.Contains(m.content, "🧠 思考过程") || !strings.Contains(m.content, "让我想想") {
		t.Fatalf("第一条应是思考链: %q", m.content)
	}
	m = f2.expectSend(t)
	if !strings.Contains(m.content, "🔧 调用工具") || !strings.Contains(m.content, "exec_command") ||
		strings.Contains(m.content, "参数") {
		t.Fatalf("第二条应是工具名列表: %q", m.content)
	}
	if m = f2.expectSend(t); !strings.Contains(m.content, "最终回复") {
		t.Fatalf("第三条应是最终回复: %q", m.content)
	}

	// 只开工具：思考不推
	_, f3, _ := newInitedCfg(t, notesTurn, "user1", map[string]any{"show_tools": true})
	f3.pushC2C("user1", "查一下")
	m = f3.expectSend(t)
	if strings.Contains(m.content, "思考") {
		t.Fatalf("未开思考展示不该推思考链: %q", m.content)
	}
	if !strings.Contains(m.content, "🔧 调用工具") {
		t.Fatalf("应推工具调用: %q", m.content)
	}
	f3.expectSend(t) // 最终回复
}

// 白名单外的用户被拒绝，无任何回复。
func TestWhitelistBlocksStrangers(t *testing.T) {
	_, f, _ := newInited(t, func(context.Context, string, string) (string, error) {
		t.Error("陌生人不该触发对话")
		return "", nil
	}, "user1")
	f.pushC2C("stranger", "在吗")
	f.expectNoSend(t, 500*time.Millisecond)
}

// /help /status /new 命令链路；/new 换绑会话。
func TestCommands(t *testing.T) {
	var turns atomic.Int32
	runTurn := func(_ context.Context, sid, input string) (string, error) {
		turns.Add(1)
		return "ok", nil
	}
	p, f, _ := newInited(t, runTurn, "user1")

	f.pushC2C("user1", "/help")
	if m := f.expectSend(t); !strings.Contains(m.content, "/compact") {
		t.Fatalf("/help 应列出命令: %s", m.content)
	}

	f.pushC2C("user1", "先聊一句")
	f.expectSend(t)
	sid1 := p.core.BoundSession("user1")
	if sid1 == "" {
		t.Fatal("对话后应有会话映射")
	}

	f.pushC2C("user1", "/new")
	if m := f.expectSend(t); !strings.Contains(m.content, "已新建会话") {
		t.Fatalf("/new 回执异常: %s", m.content)
	}
	if sid2 := p.core.BoundSession("user1"); sid2 == sid1 {
		t.Fatal("/new 后应换绑新会话")
	}

	// 措辞本身由 internal/statustext 的测试盯着，这里只验证命令确实接到了那份渲染
	f.pushC2C("user1", "/status")
	if m := f.expectSend(t); !strings.Contains(m.content, "📊 Agent 状态") ||
		!strings.Contains(m.content, "模型：testprov / m1 · 思考深度 off") ||
		!strings.Contains(m.content, "1,234 / 131,072 tokens（占用 0.9%）") {
		t.Fatalf("/status 应与 Web UI 同格式: %s", m.content)
	}

	f.pushC2C("user1", "/什么鬼")
	if m := f.expectSend(t); !strings.Contains(m.content, "未知命令") {
		t.Fatalf("未知命令回执异常: %s", m.content)
	}
}

// 确认链路：轮次中发起确认 → QQ 收到请求文案 → /apply 放行、/deny 拒绝。
func TestConfirmFlow(t *testing.T) {
	verdicts := make(chan bool, 2)
	runTurn := func(ctx context.Context, sid, input string) (string, error) {
		confirm, ok := plugin.ConfirmerFrom(ctx)
		if !ok {
			t.Error("QQ 轮次应带确认通道")
			return "", nil
		}
		approved, err := confirm(ctx, plugin.ConfirmRequest{
			Source: "exec_command", Title: "要执行危险命令", Detail: "rm -rf /tmp/x", Reason: "删除文件",
		})
		if err != nil {
			approved = false
		}
		verdicts <- approved
		return "完成", nil
	}
	_, f, _ := newInited(t, runTurn, "user1")

	// 第一轮：/apply
	f.pushC2C("user1", "删掉临时文件")
	if m := f.expectSend(t); !strings.Contains(m.content, "/apply") || !strings.Contains(m.content, "rm -rf") {
		t.Fatalf("确认请求文案异常: %s", m.content)
	}
	f.pushC2C("user1", "/apply")
	if m := f.expectSend(t); !strings.Contains(m.content, "已允许") {
		t.Fatalf("/apply 回执异常: %s", m.content)
	}
	select {
	case ok := <-verdicts:
		if !ok {
			t.Fatal("/apply 应放行")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待确认结果超时")
	}
	f.expectSend(t) // 最终回复「完成」

	// 第二轮：/deny
	f.pushC2C("user1", "再删一次")
	f.expectSend(t) // 确认请求
	f.pushC2C("user1", "/deny")
	f.expectSend(t) // 已拒绝回执
	select {
	case ok := <-verdicts:
		if ok {
			t.Fatal("/deny 应拒绝")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待确认结果超时")
	}
	f.expectSend(t)
}

// 没有等待中的确认时 /apply 得到提示。
func TestApplyWithoutPending(t *testing.T) {
	_, f, _ := newInited(t, func(context.Context, string, string) (string, error) { return "ok", nil }, "user1")
	f.pushC2C("user1", "/apply")
	if m := f.expectSend(t); !strings.Contains(m.content, "没有等待确认") {
		t.Fatalf("回执异常: %s", m.content)
	}
}

// 重复推送同一条消息只处理一次。
func TestDedup(t *testing.T) {
	var turns atomic.Int32
	runTurn := func(context.Context, string, string) (string, error) {
		turns.Add(1)
		return "ok", nil
	}
	p, f, _ := newInited(t, runTurn, "user1")

	id := f.pushC2C("user1", "你好")
	f.expectSend(t)
	// 手动重放同一 id
	p.handleInbound(context.Background(), inbound{openid: "user1", msgID: id, content: "你好"})
	f.expectNoSend(t, 500*time.Millisecond)
	if turns.Load() != 1 {
		t.Fatalf("重复消息不该重复处理，轮次=%d", turns.Load())
	}
}

// ---------- 纯函数 ----------

// ChunkMarkdown 与 ToPlainText 的测试随实现迁至 wen/internal/mdtext。

func TestDowngradeTables(t *testing.T) {
	md := strings.Join([]string{
		"前文",
		"| 姓名 | 年龄 | 城市 |",
		"|---|---|---|",
		"| 张三 | 30 | 北京 |",
		"| 李四 | 25 | 上海 |",
		"| 王五 | 28 | 广州 |",
		"| 赵六 | 22 | 深圳 |",
		"| 孙七 | 31 | 成都 |",
		"后文",
	}, "\n")
	out := downgradeTables("", md)
	if strings.Contains(out, "|---") || strings.Contains(out, "| 张三 |") {
		t.Fatalf("表格未降级:\n%s", out)
	}
	if !strings.Contains(out, "姓名: 张三 ｜ 年龄: 30 ｜ 城市: 北京") {
		t.Fatalf("键值行不符:\n%s", out)
	}
	if !strings.Contains(out, "另有 2 行数据") { // 5 行数据保留 3 行，折叠 2 行
		t.Fatalf("应输出折叠提示:\n%s", out)
	}
	// 代码块里的管道行不能被当成表格
	code := "```\n| a | b |\n```"
	if out := downgradeTables("", code); out != code {
		t.Fatalf("代码块内不该降级:\n%s", out)
	}
}

func TestSanitizeImagesAndCode(t *testing.T) {
	out := sanitizeForQQ("", "看这张 ![截图](https://x/1.png) 和 ![](https://x/2.png)")
	if strings.Contains(out, "!") || !strings.Contains(out, "[截图](https://x/1.png)") || !strings.Contains(out, "[图片](https://x/2.png)") {
		t.Fatalf("图片降级不符: %s", out)
	}

	long := "```\n" + strings.Repeat("字", 2000) + "\n```"
	out = sanitizeForQQ("", long)
	if !strings.Contains(out, "代码过长已截断，共 2000 字符") {
		t.Fatalf("长代码应截断: %s", out[:80])
	}
}

func TestReplyLimiter(t *testing.T) {
	l := newReplyLimiter()
	prev := 0
	for i := 1; i <= replyLimit; i++ {
		ok, seq := l.next("m1")
		if !ok || seq <= 0 || (i > 1 && seq != prev+1) {
			t.Fatalf("第 %d 次应放行且 seq 递增，得到 ok=%v seq=%d（上一个 %d）", i, ok, seq, prev)
		}
		prev = seq
	}
	// 重启后（换一个起点）同一 msg_id 的序号不与重启前的重叠
	if a, b := seqBase(time.Unix(1000, 0)), seqBase(time.Unix(1001, 0)); b-a < replyLimit {
		t.Fatalf("不同时刻的起点应相隔至少 %d: %d %d", replyLimit, a, b)
	}
	if ok, _ := l.next("m1"); ok {
		t.Fatal("超过 4 次应降级")
	}
	if ok, _ := l.next(""); ok {
		t.Fatal("空 msg_id 不可被动回复")
	}
	// 超窗
	l.entries["m2"] = &replyEntry{count: 1, first: time.Now().Add(-2 * time.Hour)}
	if ok, _ := l.next("m2"); ok {
		t.Fatal("超过 60 分钟应降级")
	}
}

// 另一条通道发起的轮次不该被本通道再推一遍。分通道功能会让两条通道绑在同一个
// 会话上，没有这条判定就是同一段话在两处各发一遍；投递责任归发起那一侧的路由。
func TestTurnFromAnotherChannelIsNotPushed(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "user1")
	imbot.Declare("wechat_bot", "微信")
	if err := p.core.Bind("user1", "sess-x"); err != nil {
		t.Fatal(err)
	}

	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
		SessionID: "sess-x", Origin: "wechat_bot", FinalText: "另一条通道的回复",
	})
	f.expectNoSend(t, 500*time.Millisecond)
}

// 装了分通道路由时，后台轮次也跟着人格走：不归本通道服务的会话不推。
func TestBackgroundTurnRespectsRouting(t *testing.T) {
	p, f, _ := newInited(t, noopTurn, "user1")
	if err := p.core.Bind("user1", "sess-r"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { imbot.SetRouter(nil) })

	imbot.SetRouter(func(string) string { return "wechat_bot" })
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
		SessionID: "sess-r", Origin: "heartbeat", FinalText: "该发去微信",
	})
	f.expectNoSend(t, 500*time.Millisecond)

	imbot.SetRouter(func(string) string { return "qq_bot" })
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
		SessionID: "sess-r", Origin: "heartbeat", FinalText: "该发来这里",
	})
	if m := f.expectSend(t); !strings.Contains(m.content, "该发来这里") {
		t.Fatalf("目标是本通道时应照常推送: %+v", m)
	}
}

// 网关关闭码：4009 可以 Resume，4900–4913 重新 Identify，下架、封禁与参数错误不再重连。
func TestCloseAction(t *testing.T) {
	cases := map[int]int{
		4009: closeRetry, 1006: closeRetry, 4004: closeRetry,
		4006: closeReidentify, 4007: closeReidentify, 4900: closeReidentify, 4913: closeReidentify,
		4008: closeThrottled,
		4914: closeFatal, 4915: closeFatal, 4001: closeFatal, 4012: closeFatal,
	}
	for code, want := range cases {
		if got := closeAction(&websocket.CloseError{Code: code}); got != want {
			t.Errorf("关闭码 %d: 得到 %d，应为 %d", code, got, want)
		}
	}
	if closeAction(fmt.Errorf("network down")) != closeRetry {
		t.Error("非关闭帧的错误照常重连")
	}
}

func TestMarkdownForbiddenCodes(t *testing.T) {
	for _, s := range []string{"code 40034010", "code=40034127", "Markdown not allowed"} {
		if !isMarkdownForbidden(fmt.Errorf("%s", s)) {
			t.Errorf("应认作 markdown 被拒: %s", s)
		}
	}
	if isMarkdownForbidden(fmt.Errorf("code 40054005")) {
		t.Error("序号重复不是 markdown 被拒")
	}
}

// token 只在到期前 50 秒内才换新；换回来的若仍快到期，10 秒内不再重取
func TestTokenRefreshWindow(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"t","expires_in":"30"}`))
	}))
	defer srv.Close()
	ts := newTokenSource("1", "123")
	ts.tokenURL = srv.URL
	ts.token, ts.expiresAt = "old", time.Now().Add(2*time.Minute)
	if tok, _ := ts.Get(context.Background()); tok != "old" || calls.Load() != 0 {
		t.Fatalf("离到期还远时不该重取: %s %d", tok, calls.Load())
	}
	ts.expiresAt = time.Now().Add(40 * time.Second)
	if tok, _ := ts.Get(context.Background()); tok != "t" || calls.Load() != 1 {
		t.Fatalf("进入最后 50 秒应重取: %s %d", tok, calls.Load())
	}
	for range 5 {
		_, _ = ts.Get(context.Background())
	}
	if calls.Load() != 1 {
		t.Errorf("取回来的仍快到期时不该每次都重取: %d 次", calls.Load())
	}
}

// 一轮开始时挂在入站消息上发「正在输入」，并占用一条被动回复额度（序号不与正文重复）
func TestTypingNotify(t *testing.T) {
	reply := func(context.Context, string, string) (string, error) { return "ok", nil }
	_, f, _ := newInited(t, reply, "user1")
	id := f.pushC2C("user1", "你好")
	select {
	case ty := <-f.typing:
		if ty.msgID != id || ty.seq <= 0 {
			t.Fatalf("「正在输入」应挂在入站消息上: %+v", ty)
		}
		m := f.expectSend(t)
		if m.seq == ty.seq {
			t.Fatalf("正文与「正在输入」的序号不能重复: %d", m.seq)
		}
	case <-time.After(8 * time.Second): // 入站有合并窗口，一轮要等它过去才开始
		t.Fatal("没收到「正在输入」")
	}
}

// 主动推送被拒时退一步用互动召回，一次发送只试一次
func TestPushFallsBackToWakeup(t *testing.T) {
	reply := func(context.Context, string, string) (string, error) { return "ok", nil }
	p, f, _ := newInited(t, reply, "user1")
	f.mu.Lock()
	f.rejectActive = true
	f.mu.Unlock()
	p.send(context.Background(), "user1", "早上好", "")
	m := f.expectSend(t)
	if m.content != "早上好" || m.msgID != "" {
		t.Fatalf("应以召回送达: %+v", m)
	}
	f.mu.Lock()
	n := f.wakeups
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("应只试一次召回，实际 %d 次", n)
	}
}

// 「正在输入」每条入站消息至多一次，发完之后正文还有三条被动回复额度
func TestTypingSlotOncePerMessage(t *testing.T) {
	l := newReplyLimiter()
	if ok, _ := l.typingSlot("m1"); !ok {
		t.Fatal("第一次应放行")
	}
	if ok, _ := l.typingSlot("m1"); ok {
		t.Fatal("同一条入站消息不该再发第二次")
	}
	for i := range 3 {
		if ok, _ := l.next("m1"); !ok {
			t.Fatalf("正文第 %d 段应仍能被动回复", i+1)
		}
	}
	// 已经被正文用掉额度的消息上不再发「正在输入」
	l.next("m2")
	l.next("m2")
	if ok, _ := l.typingSlot("m2"); ok {
		t.Error("剩余额度不够给正文留三条时不该发")
	}
}
