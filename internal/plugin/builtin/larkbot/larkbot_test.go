package larkbot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"wen/internal/plugin"
	"wen/internal/session"
)

// sentMsg 是假服务端收到的一次发消息请求。
type sentMsg struct {
	ReceiveID     string `json:"receive_id"`
	MsgType       string `json:"msg_type"`
	Content       string `json:"content"`
	auth          string
	receiveIDType string
}

// fakeLark 是飞书开放平台的假服务端。
type fakeLark struct {
	srv    *httptest.Server
	sent   chan sentMsg
	tokens atomic.Int64 // 取 token 的次数，用来验证缓存
	expire atomic.Int64 // 下发的 token 有效期（秒）
}

func newFakeLark(t *testing.T) *fakeLark {
	t.Helper()
	f := &fakeLark{sent: make(chan sentMsg, 32)}
	f.expire.Store(7200)
	mux := http.NewServeMux()

	mux.HandleFunc("POST /open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		n := f.tokens.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"msg":"ok","tenant_access_token":"t-%d","expire":%d}`, n, f.expire.Load())
	})

	mux.HandleFunc("POST /open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var m sentMsg
		json.NewDecoder(r.Body).Decode(&m)
		m.auth = r.Header.Get("Authorization")
		m.receiveIDType = r.URL.Query().Get("receive_id_type")
		select {
		case f.sent <- m:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"msg":"ok","data":{"message_id":"om_1"}}`)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLark) expectSend(t *testing.T) sentMsg {
	t.Helper()
	select {
	case m := <-f.sent:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("等待出站消息超时")
		return sentMsg{}
	}
}

func (f *fakeLark) expectNoSend(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.sent:
		t.Fatalf("不该有出站消息，却收到 %+v", m)
	case <-time.After(d):
	}
}

func noopTurn(_ context.Context, _, input string) (string, error) { return "收到：" + input, nil }

// newInited 起一个连到假服务端的插件；长连接被替换掉，单测不去连真平台。
func newInited(t *testing.T, ctor func() *Plugin, runTurn plugin.RunTurnFunc, whitelist string, extra map[string]any) (*Plugin, *fakeLark) {
	t.Helper()
	f := newFakeLark(t)
	dir := t.TempDir()
	store, err := session.NewStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	p := ctor()
	p.startWS = func(context.Context, string, string, string) {} // 不连长连接

	cfg := map[string]any{
		"app_id":     "cli_test",
		"app_secret": "123",
		"api_base":   f.srv.URL,
		"whitelist":  whitelist,
	}
	for k, v := range extra {
		cfg[k] = v
	}
	ictx := plugin.InitContext{
		StateDir:   filepath.Join(dir, "state"),
		RunTurn:    runTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
		Sessions:   store,
	}
	if err := p.Init(ictx, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(p.Stop)
	return p, f
}

// textEvent 造一条单聊文本事件。
func textEvent(openID, text string) *larkim.P2MessageReceiveV1 {
	return event(openID, "user", "p2p", "text", fmt.Sprintf(`{"text":%q}`, text), "om_"+text)
}

func event(openID, senderType, chatType, msgType, content, msgID string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderType: &senderType,
				SenderId:   &larkim.UserId{OpenId: &openID},
			},
			Message: &larkim.EventMessage{
				MessageId:   &msgID,
				ChatType:    &chatType,
				MessageType: &msgType,
				Content:     &content,
			},
		},
	}
}

// ---------- 两个变体 ----------

// 飞书与 Lark 是同一套实现的两次实例化，差异只有这几项；插件名不同意味着
// StateDir 不同，两边的凭证与会话绑定因此天然隔离。
func TestVariants(t *testing.T) {
	cases := []struct {
		ctor     func() *Plugin
		name     string
		base     string
		inDesc   string
		notDesc  string
		consoleS string
	}{
		{NewFeishu, "feishu_bot", lark.FeishuBaseUrl, "飞书", "Lark", "feishu.cn"},
		{NewLark, "lark_bot", lark.LarkBaseUrl, "Lark", "飞书", "larksuite.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.ctor()
			if p.Name() != c.name {
				t.Errorf("Name = %q，想要 %q", p.Name(), c.name)
			}
			if !strings.Contains(p.Description(), c.inDesc) || strings.Contains(p.Description(), c.notDesc) {
				t.Errorf("描述里的产品名不对: %q", p.Description())
			}
			var base, secretDesc, appIDDesc string
			for _, f := range p.ConfigFields() {
				switch f.Key {
				case "api_base":
					base, _ = f.Default.(string)
				case "app_secret":
					secretDesc = f.Description
				case "app_id":
					appIDDesc = f.Description
				}
			}
			// 两边的开发者后台是不同的站点，指错了地方用户建的应用连不上
			if !strings.Contains(appIDDesc, c.consoleS) {
				t.Errorf("应指向 %s 的开发者后台: %q", c.consoleS, appIDDesc)
			}
			if base != c.base {
				t.Errorf("默认 api_base = %q，想要 %q", base, c.base)
			}
			if !strings.Contains(secretDesc, c.inDesc) {
				t.Errorf("配置项文案应带产品名: %q", secretDesc)
			}
			if p.Category() != plugin.CategoryChannel {
				t.Errorf("分组应为消息通道: %q", p.Category())
			}
		})
	}
	if NewFeishu().Name() == NewLark().Name() {
		t.Fatal("两个变体必须有不同的插件名，否则共用 StateDir，凭证与会话绑定会互相覆盖")
	}
}

func TestInitRequiresCredentials(t *testing.T) {
	p := NewFeishu()
	p.startWS = func(context.Context, string, string, string) {}
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, nil); err == nil {
		t.Fatal("没有 App ID / Secret 时应拒绝启用")
	}
}

// ---------- 消息内容 ----------

func TestParseContent(t *testing.T) {
	cases := []struct {
		name           string
		msgType        string
		content        string
		wantText       string
		wantNoteSubstr string
	}{
		{"文本", "text", `{"text":"你好"}`, "你好", ""},
		{"剥掉 @ 占位符", "text", `{"text":"@_user_1 你好"}`, "你好", ""},
		{"空文本", "text", `{"text":"   "}`, "", "没有收到文字"},
		{"坏 JSON", "text", `{`, "", "解析失败"},
		{"图片", "image", `{"image_key":"x"}`, "", "暂不支持"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, note := parseContent("", c.msgType, c.content)
			if text != c.wantText {
				t.Errorf("正文 = %q，想要 %q", text, c.wantText)
			}
			if c.wantNoteSubstr == "" && note != "" {
				t.Errorf("不该有提示: %q", note)
			}
			if c.wantNoteSubstr != "" && !strings.Contains(note, c.wantNoteSubstr) {
				t.Errorf("提示 = %q，应含 %q", note, c.wantNoteSubstr)
			}
		})
	}
}

// content 是「JSON 序列化后的字符串」，不是嵌套对象——这是飞书接口的形状。
func TestBuildPayload(t *testing.T) {
	typ, content := buildPayload("", formatPlain, "**重点**")
	if typ != "text" {
		t.Fatalf("纯文本模式的 msg_type = %q", typ)
	}
	var tc struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &tc); err != nil {
		t.Fatalf("content 应是可解析的 JSON 字符串: %v", err)
	}
	if strings.Contains(tc.Text, "**") {
		t.Errorf("纯文本模式应剥掉 markdown 标记: %q", tc.Text)
	}

	typ, content = buildPayload("", formatCard, "**重点**")
	if typ != "interactive" {
		t.Fatalf("卡片模式的 msg_type = %q", typ)
	}
	if !strings.Contains(content, "lark_md") {
		t.Errorf("卡片应用 lark_md 渲染: %s", content)
	}
	if !strings.Contains(content, `**重点**`) {
		t.Errorf("卡片模式应原样带上 markdown: %s", content)
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatalf("卡片 content 应是可解析的 JSON 字符串: %v", err)
	}
}

// ---------- token ----------

func TestTokenCachedAndRefreshed(t *testing.T) {
	f := newFakeLark(t)
	ts := newTokenSource("cli_test", "123", f.srv.URL, &http.Client{Timeout: 5 * time.Second})

	for range 3 {
		if _, err := ts.Get(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.tokens.Load(); got != 1 {
		t.Fatalf("有效期内应复用同一个 token，实际取了 %d 次", got)
	}

	// 官方建议剩余不足 30 分钟就换新：给一个短有效期，下一次必须重新取
	f.expire.Store(60)
	ts.expiresAt = time.Now().Add(time.Minute)
	if _, err := ts.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.tokens.Load(); got != 2 {
		t.Fatalf("临近过期应刷新，实际取了 %d 次", got)
	}
}

func TestTokenErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":10003,"msg":"invalid app_secret"}`)
	}))
	t.Cleanup(srv.Close)
	ts := newTokenSource("cli_test", "bad", srv.URL, &http.Client{Timeout: 5 * time.Second})
	_, err := ts.Get(context.Background())
	if err == nil || !strings.Contains(err.Error(), "10003") {
		t.Fatalf("业务错误码应暴露出来: %v", err)
	}
}

// ---------- 收发 ----------

func TestChatRoundTrip(t *testing.T) {
	p, f := newInited(t, NewFeishu, noopTurn, "ou_me", nil)
	p.onMessage(textEvent("ou_me", "你好"))

	m := f.expectSend(t)
	if m.ReceiveID != "ou_me" || !strings.Contains(m.Content, "收到：你好") {
		t.Fatalf("回复异常: %+v", m)
	}
	if m.MsgType != "interactive" {
		t.Fatalf("默认应发卡片: %+v", m)
	}
	if m.receiveIDType != "open_id" {
		t.Fatalf("receive_id_type 应为 open_id: %q", m.receiveIDType)
	}
	if !strings.HasPrefix(m.auth, "Bearer t-") {
		t.Fatalf("鉴权头形式不对: %q", m.auth)
	}
}

func TestPlainFormat(t *testing.T) {
	p, f := newInited(t, NewFeishu, func(context.Context, string, string) (string, error) {
		return "带 **格式** 的回复", nil
	}, "ou_me", map[string]any{"format": formatPlain})

	p.onMessage(textEvent("ou_me", "你好"))
	m := f.expectSend(t)
	if m.MsgType != "text" {
		t.Fatalf("纯文本模式应发 text: %+v", m)
	}
	if strings.Contains(m.Content, "**") {
		t.Fatalf("纯文本模式不该留 markdown 标记: %s", m.Content)
	}
}

func TestWhitelistAndP2POnly(t *testing.T) {
	p, f := newInited(t, NewFeishu, noopTurn, "ou_me", nil)

	p.onMessage(textEvent("ou_stranger", "我是陌生人"))                                  // 白名单外
	p.onMessage(event("ou_me", "user", "group", "text", `{"text":"群里说话"}`, "om_g")) // 群聊
	p.onMessage(event("ou_me", "bot", "p2p", "text", `{"text":"自己的回显"}`, "om_b"))   // 机器人回显
	f.expectNoSend(t, 800*time.Millisecond)

	p.onMessage(textEvent("ou_me", "我是自己人"))
	if m := f.expectSend(t); m.ReceiveID != "ou_me" {
		t.Fatalf("白名单内的单聊应被处理: %+v", m)
	}
}

func TestUnsupportedTypeGetsNote(t *testing.T) {
	p, f := newInited(t, NewFeishu, noopTurn, "ou_me", nil)
	p.onMessage(event("ou_me", "user", "p2p", "image", `{"image_key":"x"}`, "om_i"))
	if m := f.expectSend(t); !strings.Contains(m.Content, "暂不支持") {
		t.Fatalf("应回一句暂不支持: %+v", m)
	}
}

func TestDedupByMessageID(t *testing.T) {
	var turns atomic.Int64
	p, f := newInited(t, NewFeishu, func(context.Context, string, string) (string, error) {
		turns.Add(1)
		return "ok", nil
	}, "ou_me", nil)

	ev := textEvent("ou_me", "你好")
	p.onMessage(ev)
	f.expectSend(t)
	p.onMessage(ev) // 同一个 message_id 重放
	f.expectNoSend(t, 500*time.Millisecond)
	if turns.Load() != 1 {
		t.Fatalf("重复消息不该重复处理，轮次=%d", turns.Load())
	}
}

func TestCommandsReachCore(t *testing.T) {
	p, f := newInited(t, NewFeishu, noopTurn, "ou_me", nil)
	p.onMessage(textEvent("ou_me", "/help"))
	if m := f.expectSend(t); !strings.Contains(m.Content, "/compact") {
		t.Fatalf("/help 应列出命令: %s", m.Content)
	}
}

// 后台轮次（心跳等）落到本通道绑定的会话时，把最终文本推给绑定用户；
// 前台轮次与本插件自己发起的轮次不推。
func TestBackgroundTurnPush(t *testing.T) {
	p, f := newInited(t, NewLark, noopTurn, "ou_me", nil)
	if err := p.core.Bind("ou_me", "sess-hb"); err != nil {
		t.Fatal(err)
	}

	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
		SessionID: "sess-hb", Origin: "heartbeat", FinalText: "早上好，有新进展。",
	})
	if m := f.expectSend(t); m.ReceiveID != "ou_me" || !strings.Contains(m.Content, "早上好") {
		t.Fatalf("后台轮次结果应推给绑定用户: %+v", m)
	}

	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "sess-hb", Origin: "", FinalText: "前台"})
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "sess-hb", Origin: "lark_bot", FinalText: "自己"})
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "sess-other", Origin: "heartbeat", FinalText: "无关"})
	f.expectNoSend(t, 500*time.Millisecond)
}

// 超长回复要分段，且每段都发得出去。
func TestLongReplyChunks(t *testing.T) {
	long := strings.Repeat("这是一句很长的话。", 800)
	p, f := newInited(t, NewFeishu, func(context.Context, string, string) (string, error) {
		return long, nil
	}, "ou_me", nil)

	p.onMessage(textEvent("ou_me", "来点长的"))
	first := f.expectSend(t)
	if !strings.Contains(first.Content, "(1/") {
		t.Fatalf("多段时应带序号: %.60q", first.Content)
	}
	f.expectSend(t) // 至少还有第二段
}

// Init 可重入：改配置会重新 Init，此时可能有正在处理的消息。
func TestInitIsReentrant(t *testing.T) {
	var wg sync.WaitGroup
	p, f := newInited(t, NewFeishu, noopTurn, "ou_me", nil)
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.onMessage(textEvent("ou_me", "第一条"))
	}()
	wg.Wait()
	f.expectSend(t)

	dir := t.TempDir()
	store, _ := session.NewStore(filepath.Join(dir, "sessions"))
	err := p.Init(plugin.InitContext{
		StateDir:   filepath.Join(dir, "state"),
		RunTurn:    noopTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
		Sessions:   store,
	}, map[string]any{
		"app_id": "cli_test", "app_secret": "123",
		"api_base": f.srv.URL, "whitelist": "ou_me",
	})
	if err != nil {
		t.Fatalf("重新 Init 应成功: %v", err)
	}
	p.onMessage(textEvent("ou_me", "第二条"))
	if m := f.expectSend(t); !strings.Contains(m.Content, "第二条") {
		t.Fatalf("重新 Init 后应继续工作: %+v", m)
	}
}

// 产品名一个是中文一个是拉丁文，同一句模板套两边，空格得按相邻字符的类别补。
func TestJoinCN(t *testing.T) {
	cases := []struct {
		parts []string
		want  string
	}{
		{[]string{"接入", "飞书", "机器人"}, "接入飞书机器人"},
		{[]string{"接入", "Lark", "机器人"}, "接入 Lark 机器人"},
		{[]string{"在", "Lark", "开发者后台（https://x）建应用"}, "在 Lark 开发者后台（https://x）建应用"},
		{[]string{"Lark", "的普通文本消息"}, "Lark 的普通文本消息"},
		{[]string{"飞书", "的普通文本消息"}, "飞书的普通文本消息"},
		// 标点两侧不补：「（」不是汉字，「Lark」前面不该多一个空格
		{[]string{"（", "Lark", "开发者后台"}, "（Lark 开发者后台"},
		{[]string{"推送到", "Lark", "；关闭"}, "推送到 Lark；关闭"},
		{[]string{"", "飞书", ""}, "飞书"},
	}
	for _, c := range cases {
		if got := joinCN(c.parts...); got != c.want {
			t.Errorf("joinCN(%q)\n= %q\n想要 %q", c.parts, got, c.want)
		}
	}
}
