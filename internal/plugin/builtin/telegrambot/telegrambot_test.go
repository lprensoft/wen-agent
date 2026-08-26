package telegrambot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wen/internal/plugin"
	"wen/internal/session"
)

const testToken = "123:abc"

// sentMsg 是假服务端收到的一次 sendMessage。
type sentMsg struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
	Reply     struct {
		MessageID int64 `json:"message_id"`
	} `json:"reply_parameters"`
}

// fakeTG 是 Telegram Bot API 的假服务端。
type fakeTG struct {
	srv *httptest.Server

	mu      sync.Mutex
	pending []update // 待下发的更新
	nextID  int64
	offsets []int64 // 每次 getUpdates 带来的 offset，用来验证游标推进

	sent chan sentMsg

	// 下一次 sendMessage 的强制结果（用完即弃）
	failNext atomic.Value // func() (int, string, int) -> code, description, retryAfter
	actions  atomic.Int64
}

func newFakeTG(t *testing.T) *fakeTG {
	t.Helper()
	f := &fakeTG{sent: make(chan sentMsg, 64)}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /bot"+testToken+"/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Offset int64 `json:"offset"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		f.mu.Lock()
		f.offsets = append(f.offsets, req.Offset)
		var out []update
		for _, u := range f.pending {
			if u.UpdateID >= req.Offset {
				out = append(out, u)
			}
		}
		f.pending = nil
		f.mu.Unlock()

		if len(out) == 0 {
			// 模拟长轮询窗口内无消息，但别真等 50 秒
			time.Sleep(30 * time.Millisecond)
		}
		writeOK(w, out)
	})

	mux.HandleFunc("POST /bot"+testToken+"/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var m sentMsg
		json.NewDecoder(r.Body).Decode(&m)
		if fn, ok := f.failNext.Load().(func() (int, string, int)); ok && fn != nil {
			f.failNext.Store((func() (int, string, int))(nil))
			code, desc, retry := fn()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":%q,"parameters":{"retry_after":%d}}`,
				code, desc, retry)
			return
		}
		select {
		case f.sent <- m:
		default:
		}
		writeOK(w, map[string]any{"message_id": 1})
	})

	mux.HandleFunc("POST /bot"+testToken+"/sendChatAction", func(w http.ResponseWriter, r *http.Request) {
		f.actions.Add(1)
		writeOK(w, true)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeOK(w http.ResponseWriter, result any) {
	raw, _ := json.Marshal(result)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"result":%s}`, raw)
}

// push 排一条私聊文本消息，返回它的 update_id。
func (f *fakeTG) push(chatID int64, text string) int64 {
	return f.pushChat(chatID, "private", text)
}

func (f *fakeTG) pushChat(chatID int64, chatType, text string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	var u update
	u.UpdateID = f.nextID
	u.Message = &struct {
		MessageID int64 `json:"message_id"`
		Date      int64 `json:"date"`
		Chat      struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		Text    string `json:"text"`
		Caption string `json:"caption"`
	}{MessageID: f.nextID * 10, Text: text}
	u.Message.Chat.ID = chatID
	u.Message.Chat.Type = chatType
	f.pending = append(f.pending, u)
	return f.nextID
}

func (f *fakeTG) expectSend(t *testing.T) sentMsg {
	t.Helper()
	select {
	case m := <-f.sent:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("等待出站消息超时")
		return sentMsg{}
	}
}

func (f *fakeTG) expectNoSend(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.sent:
		t.Fatalf("不该有出站消息，却收到 %+v", m)
	case <-time.After(d):
	}
}

func (f *fakeTG) lastOffsets() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.offsets...)
}

func noopTurn(_ context.Context, _, input string) (string, error) { return "收到：" + input, nil }

// newInited 起一个连到假服务端的插件。
func newInited(t *testing.T, runTurn plugin.RunTurnFunc, whitelist string, extra map[string]any) (*Plugin, *fakeTG) {
	t.Helper()
	f := newFakeTG(t)
	dir := t.TempDir()
	store, err := session.NewStore(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	p := New()
	cfg := map[string]any{
		"bot_token": testToken,
		"api_base":  f.srv.URL,
		"whitelist": whitelist,
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

// ---------- MarkdownV2 转换 ----------

func TestToMarkdownV2(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"保留字全转义", "1. 见 a-b_c (x).", `1\. 见 a\-b\_c \(x\)\.`},
		{"加粗", "这是 **重点** 内容", `这是 *重点* 内容`},
		{"下划线加粗", "这是 __重点__ 内容", `这是 *重点* 内容`},
		{"斜体", "这是 *轻* 内容", `这是 _轻_ 内容`},
		{"删除线", "这是 ~~没了~~ 内容", `这是 ~没了~ 内容`},
		{"行内代码里的保留字不转义", "用 `a-b_c` 命令", "用 `a-b_c` 命令"},
		{"链接", "见 [文档](https://x.dev/a_b)", `见 [文档](https://x.dev/a_b)`},
		{"图片降级成链接", "![](https://x.dev/p.png)", `[图片](https://x.dev/p.png)`},
		{"标题转加粗", "## 小标题", `*小标题*`},
		{"标题里的强调剥掉", "## **小标题**", `*小标题*`},
		{"列表换圆点", "- 第一项", `• 第一项`},
		{"引用", "> 引文 x.", `>引文 x\.`},
		{"分隔线", "---", hrLine},
		{"snake_case 不当斜体", "字段 user_name_id 有效", `字段 user\_name\_id 有效`},
		{"孤立星号按字面量", "2 * 3 = 6", `2 \* 3 \= 6`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toMarkdownV2("", c.in); got != c.want {
				t.Errorf("toMarkdownV2(%q)\n= %q\n想要 %q", c.in, got, c.want)
			}
		})
	}
}

func TestToMarkdownV2CodeBlock(t *testing.T) {
	got := toMarkdownV2("", "说明：\n```go\na := b-c\n```\n完")
	want := "说明：\n```go\na := b-c\n```\n完"
	if got != want {
		t.Errorf("代码块内容不该被转义\n= %q\n想要 %q", got, want)
	}
}

// 围栏不成对会让 Telegram 整条拒收，得自己补上。
func TestToMarkdownV2ClosesUnterminatedFence(t *testing.T) {
	got := toMarkdownV2("", "```\nfoo")
	if strings.Count(got, "```") != 2 {
		t.Errorf("未闭合的代码块应补上围栏: %q", got)
	}
}

// ---------- 代理 ----------

func TestBuildTransport(t *testing.T) {
	if tr, err := buildTransport("", ""); err != nil || tr != nil {
		t.Errorf("留空应直连: %v %v", tr, err)
	}
	for _, ok := range []string{"http://127.0.0.1:7890", "https://127.0.0.1:7890", "socks5://127.0.0.1:1080"} {
		if _, err := buildTransport("", ok); err != nil {
			t.Errorf("%s 应被接受: %v", ok, err)
		}
	}
	for _, bad := range []string{"127.0.0.1:1080", "ftp://x:1", "socks5://"} {
		if _, err := buildTransport("", bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func TestInitRequiresToken(t *testing.T) {
	if err := New().Init(plugin.InitContext{StateDir: t.TempDir()}, nil); err == nil {
		t.Fatal("没有 Bot Token 时应拒绝启用")
	}
}

// ---------- 收发 ----------

func TestChatRoundTrip(t *testing.T) {
	_, f := newInited(t, noopTurn, "42", nil)
	f.push(42, "你好")
	m := f.expectSend(t)
	if m.ChatID != "42" || !strings.Contains(m.Text, "收到：你好") {
		t.Fatalf("回复异常: %+v", m)
	}
	if m.ParseMode != "MarkdownV2" {
		t.Fatalf("默认应按 MarkdownV2 发送: %+v", m)
	}
	if m.Reply.MessageID == 0 {
		t.Fatalf("首条回复应挂在原消息上: %+v", m)
	}
}

func TestWhitelistAndPrivateOnly(t *testing.T) {
	_, f := newInited(t, noopTurn, "42", nil)
	f.push(99, "我是陌生人") // 白名单外
	f.pushChat(42, "group", "群里说话")
	f.expectNoSend(t, 800*time.Millisecond)

	f.push(42, "我是自己人")
	if m := f.expectSend(t); m.ChatID != "42" {
		t.Fatalf("白名单内的私聊应被处理: %+v", m)
	}
}

// 游标必须推进并落盘：只活在内存里的话，重启会把已处理的消息再收一遍。
func TestOffsetAdvancesAndPersists(t *testing.T) {
	p, f := newInited(t, noopTurn, "42", nil)
	id := f.push(42, "第一条")
	f.expectSend(t)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		offs := f.lastOffsets()
		if len(offs) > 0 && offs[len(offs)-1] == id+1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if offs := f.lastOffsets(); len(offs) == 0 || offs[len(offs)-1] != id+1 {
		t.Fatalf("游标应推进到 %d: %v", id+1, offs)
	}

	p.mu.Lock()
	stateDir := p.stateDir
	p.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(stateDir, "offset"))
	if err != nil {
		t.Fatalf("游标应落盘: %v", err)
	}
	if strings.TrimSpace(string(raw)) != fmt.Sprint(id+1) {
		t.Fatalf("落盘的游标 = %q，想要 %d", raw, id+1)
	}
}

// MarkdownV2 解析失败时当场降级纯文本重发，消息不能丢。
func TestParseErrorDowngradesToPlain(t *testing.T) {
	_, f := newInited(t, func(context.Context, string, string) (string, error) {
		return "带 **格式** 的回复", nil
	}, "42", nil)

	f.failNext.Store(func() (int, string, int) {
		return 400, "Bad Request: can't parse entities: character '-' is reserved", 0
	})
	f.push(42, "你好")

	m := f.expectSend(t)
	if m.ParseMode != "" {
		t.Fatalf("降级后应发纯文本: %+v", m)
	}
	if !strings.Contains(m.Text, "格式") {
		t.Fatalf("降级重发的内容不对: %+v", m)
	}

	// 该用户此后在 TTL 内直接走纯文本，不再重试 MarkdownV2
	f.push(42, "再来一条")
	if m := f.expectSend(t); m.ParseMode != "" {
		t.Fatalf("同一用户后续应继续纯文本: %+v", m)
	}
}

// 限流时按官方给的秒数等一次再重试。
func TestRetryAfter(t *testing.T) {
	_, f := newInited(t, noopTurn, "42", nil)
	f.failNext.Store(func() (int, string, int) {
		return 429, "Too Many Requests: retry after 1", 1
	})
	start := time.Now()
	f.push(42, "你好")
	m := f.expectSend(t)
	if !strings.Contains(m.Text, "收到：你好") {
		t.Fatalf("限流后应重发原内容: %+v", m)
	}
	if time.Since(start) < time.Second {
		t.Fatal("应按 retry_after 等待后再重试")
	}
}

// 超过 4096 的回复要分段，且每段都发得出去。
func TestLongReplyChunks(t *testing.T) {
	long := strings.Repeat("这是一句很长的话。", 800) // 远超 4096
	_, f := newInited(t, func(context.Context, string, string) (string, error) {
		return long, nil
	}, "42", nil)

	f.push(42, "来点长的")
	first := f.expectSend(t)
	if !strings.HasPrefix(first.Text, `\(1/`) && !strings.HasPrefix(first.Text, "(1/") {
		t.Fatalf("多段时应带序号: %.40q", first.Text)
	}
	if len([]rune(first.Text)) > 4096 {
		t.Fatalf("单段超过平台上限: %d", len([]rune(first.Text)))
	}
	second := f.expectSend(t)
	if second.Reply.MessageID != 0 {
		t.Fatal("只有第一段该挂在原消息上回复")
	}
}

func TestUnsupportedMediaGetsNote(t *testing.T) {
	_, f := newInited(t, noopTurn, "42", nil)
	f.push(42, "") // 无文本无 caption
	if m := f.expectSend(t); !strings.Contains(m.Text, "暂不支持") {
		t.Fatalf("应回一句暂不支持: %+v", m)
	}
}

func TestTypingAction(t *testing.T) {
	_, f := newInited(t, noopTurn, "42", nil)
	f.push(42, "你好")
	f.expectSend(t)
	if f.actions.Load() == 0 {
		t.Fatal("跑一轮对话前应发「正在输入」")
	}
}

// 后台轮次（心跳等）落到 Telegram 绑定的会话时，把最终文本推给绑定用户；
// 前台轮次与本插件自己发起的轮次不推。
func TestBackgroundTurnPush(t *testing.T) {
	p, f := newInited(t, noopTurn, "42", nil)
	if err := p.core.Bind("42", "sess-hb"); err != nil {
		t.Fatal(err)
	}

	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
		SessionID: "sess-hb", Origin: "heartbeat", FinalText: "早上好，有新进展。",
	})
	if m := f.expectSend(t); m.ChatID != "42" || !strings.Contains(m.Text, "早上好") {
		t.Fatalf("后台轮次结果应推给绑定用户: %+v", m)
	}

	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "sess-hb", Origin: "", FinalText: "前台"})
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "sess-hb", Origin: "telegram_bot", FinalText: "自己"})
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "sess-other", Origin: "heartbeat", FinalText: "无关"})
	f.expectNoSend(t, 500*time.Millisecond)
}

func TestCommandsReachCore(t *testing.T) {
	_, f := newInited(t, noopTurn, "42", nil)
	f.push(42, "/help")
	if m := f.expectSend(t); !strings.Contains(m.Text, "/compact") {
		t.Fatalf("/help 应列出命令: %s", m.Text)
	}
}
