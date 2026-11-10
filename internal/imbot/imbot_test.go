package imbot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
)

// fakeSender 记下所有出站消息。
type fakeSender struct {
	mu   sync.Mutex
	sent []sentMsg
	ch   chan sentMsg
}

type sentMsg struct{ userID, text, replyTo string }

func newFakeSender() *fakeSender {
	return &fakeSender{ch: make(chan sentMsg, 64)}
}

func (f *fakeSender) Send(_ context.Context, userID, text, replyTo string) {
	m := sentMsg{userID, text, replyTo}
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	select {
	case f.ch <- m:
	default:
	}
}

// expect 等一条出站消息。
func (f *fakeSender) expect(t *testing.T) sentMsg {
	t.Helper()
	select {
	case m := <-f.ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("等待出站消息超时")
		return sentMsg{}
	}
}

func (f *fakeSender) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.ch:
		t.Fatalf("不该有出站消息，却收到 %+v", m)
	case <-time.After(d):
	}
}

// fakeSessions 是最小的 SessionQuery：认得所有以 sess- 开头的会话。
type fakeSessions struct{}

func (fakeSessions) LastActive() (string, time.Time, error) { return "", time.Time{}, nil }
func (fakeSessions) LastInteraction() (time.Time, error)    { return time.Time{}, nil }
func (fakeSessions) Exists(id string) bool                  { return strings.HasPrefix(id, "sess-") }

// newCore 造一个已启动的 Core，白名单只放行 u1。
func newCore(t *testing.T, runTurn plugin.RunTurnFunc, tweak func(*Config)) (*Core, *fakeSender) {
	t.Helper()
	f := newFakeSender()
	var seq atomic.Int64
	cfg := Config{
		PluginName:     "test_bot",
		Sender:         f,
		StateDir:       t.TempDir(),
		ConfirmTimeout: 2 * time.Second,
		TurnTimeout:    5 * time.Second,
		Allow:          func(u string) bool { return u == "u1" },
		RunTurn:        runTurn,
		NewSession: func() (string, error) {
			return fmt.Sprintf("sess-%d", seq.Add(1)), nil
		},
		Sessions: fakeSessions{},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Start(context.Background())
	t.Cleanup(c.Stop)
	return c, f
}

func noopTurn(_ context.Context, _, input string) (string, error) { return "收到：" + input, nil }

func TestNewRequiresCapabilities(t *testing.T) {
	base := func() Config {
		return Config{Sender: newFakeSender(), StateDir: t.TempDir(),
			RunTurn: noopTurn, NewSession: func() (string, error) { return "sess-1", nil },
			Sessions: fakeSessions{}}
	}
	if _, err := New(base()); err != nil {
		t.Fatalf("完整配置应能构造: %v", err)
	}
	for name, break_ := range map[string]func(*Config){
		"缺 Sender":     func(c *Config) { c.Sender = nil },
		"缺持久化目录":       func(c *Config) { c.StateDir = "" },
		"缺 RunTurn":    func(c *Config) { c.RunTurn = nil },
		"缺 NewSession": func(c *Config) { c.NewSession = nil },
		"缺 Sessions":   func(c *Config) { c.Sessions = nil },
	} {
		cfg := base()
		break_(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s 时应拒绝构造", name)
		}
	}
}

func TestHandlePassesImagesToRunTurnImages(t *testing.T) {
	var got []llm.ImagePart
	c, f := newCore(t, func(context.Context, string, string) (string, error) {
		t.Fatal("images should use RunTurnImages")
		return "", nil
	}, func(cfg *Config) {
		cfg.RunTurnImages = func(_ context.Context, _, _ string, images []llm.ImagePart) (string, error) {
			got = images
			return "收到图片", nil
		}
	})
	c.Handle(context.Background(), Message{UserID: "u1", Text: "看图", Images: []llm.ImagePart{{MediaType: "image/png"}}})
	if m := f.expect(t); !strings.Contains(m.text, "收到图片") {
		t.Fatalf("reply = %+v", m)
	}
	if len(got) != 1 || got[0].MediaType != "image/png" {
		t.Fatalf("images = %+v", got)
	}
}

func TestWhitelistRejects(t *testing.T) {
	c, f := newCore(t, noopTurn, nil)
	c.Handle(context.Background(), Message{UserID: "stranger", Text: "你好"})
	f.expectNone(t, 300*time.Millisecond)
}

// Allow 为 nil 必须是「谁都不放行」，而不是「谁都放行」——某条通道忘了实现
// 就对全网开放是不可接受的失败方向。
func TestNilAllowRejectsEveryone(t *testing.T) {
	c, f := newCore(t, noopTurn, func(cfg *Config) { cfg.Allow = nil })
	c.Handle(context.Background(), Message{UserID: "u1", Text: "你好"})
	f.expectNone(t, 300*time.Millisecond)
}

func TestDedup(t *testing.T) {
	var turns atomic.Int64
	c, f := newCore(t, func(ctx context.Context, _, input string) (string, error) {
		turns.Add(1)
		return "ok", nil
	}, nil)

	msg := Message{UserID: "u1", DedupID: "m1", Text: "你好"}
	c.Handle(context.Background(), msg)
	f.expect(t)
	c.Handle(context.Background(), msg) // 重放
	f.expectNone(t, 300*time.Millisecond)
	if turns.Load() != 1 {
		t.Fatalf("重复消息不该重复处理，轮次=%d", turns.Load())
	}
}

func TestChatAndCommands(t *testing.T) {
	c, f := newCore(t, noopTurn, nil)
	ctx := context.Background()

	c.Handle(ctx, Message{UserID: "u1", Text: "你好", ReplyTo: "r1"})
	if m := f.expect(t); m.text != "收到：你好" || m.replyTo != "r1" {
		t.Fatalf("对话回复异常: %+v", m)
	}

	c.Handle(ctx, Message{UserID: "u1", Text: "/help"})
	if m := f.expect(t); !strings.Contains(m.text, "/compact") {
		t.Fatalf("/help 应列出命令: %s", m.text)
	}

	sid1 := c.BoundSession("u1")
	if sid1 == "" {
		t.Fatal("对话后应有会话映射")
	}
	c.Handle(ctx, Message{UserID: "u1", Text: "/new"})
	if m := f.expect(t); !strings.Contains(m.text, "已新建会话") {
		t.Fatalf("/new 回执异常: %s", m.text)
	}
	if sid2 := c.BoundSession("u1"); sid2 == sid1 {
		t.Fatal("/new 后应换绑新会话")
	}

	c.Handle(ctx, Message{UserID: "u1", Text: "/nope"})
	if m := f.expect(t); !strings.Contains(m.text, "未知命令") {
		t.Fatalf("未知命令回复异常: %s", m.text)
	}
}

func TestSessionBusy(t *testing.T) {
	c, f := newCore(t, func(context.Context, string, string) (string, error) {
		return "", plugin.ErrSessionBusy
	}, nil)
	c.Handle(context.Background(), Message{UserID: "u1", Text: "你好"})
	if m := f.expect(t); !strings.Contains(m.text, "正忙") {
		t.Fatalf("会话忙时应有专门提示: %s", m.text)
	}
}

func TestEmptyTextWithNote(t *testing.T) {
	c, f := newCore(t, noopTurn, nil)
	c.Handle(context.Background(), Message{UserID: "u1", Text: "  ", Note: "暂不支持该消息类型。"})
	if m := f.expect(t); m.text != "暂不支持该消息类型。" {
		t.Fatalf("应回提示: %+v", m)
	}
	// 没有提示的空消息静默丢弃
	c.Handle(context.Background(), Message{UserID: "u1", Text: ""})
	f.expectNone(t, 300*time.Millisecond)
}

// /apply 必须绕开串行队列直接投给确认代理：worker 此刻正阻塞在等确认上，
// 排队就是自锁。这是抽骨架时最容易改坏的一条。
func TestApplyDeliversWhileWorkerBlocked(t *testing.T) {
	confirmed := make(chan bool, 1)
	c, f := newCore(t, func(ctx context.Context, _, _ string) (string, error) {
		fn, ok := plugin.ConfirmerFrom(ctx)
		if !ok {
			return "", fmt.Errorf("没有确认通道")
		}
		ok2, err := fn(ctx, plugin.ConfirmRequest{Title: "执行命令", Detail: "rm -rf /"})
		if err != nil {
			return "", err
		}
		confirmed <- ok2
		return "执行完毕", nil
	}, nil)

	ctx := context.Background()
	c.Handle(ctx, Message{UserID: "u1", DedupID: "m1", Text: "删点东西"})
	if m := f.expect(t); !strings.Contains(m.text, "需要你确认") || !strings.Contains(m.text, "rm -rf /") {
		t.Fatalf("应先发出确认请求: %s", m.text)
	}

	// worker 正卡在等确认；这条 /apply 若排进队列就永远送不到
	c.Handle(ctx, Message{UserID: "u1", DedupID: "m2", Text: "/apply"})
	// 回执排在放行之前：否则用户会先看到执行结果再看到「已允许」
	if m := f.expect(t); !strings.Contains(m.text, "已允许") {
		t.Fatalf("应先发回执: %s", m.text)
	}
	select {
	case got := <-confirmed:
		if !got {
			t.Fatal("/apply 应放行")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("确认没有送达（很可能是排进了串行队列造成自锁）")
	}
	if m := f.expect(t); m.text != "执行完毕" {
		t.Fatalf("放行后应继续执行: %s", m.text)
	}
}

func TestDenyAndNoPending(t *testing.T) {
	c, f := newCore(t, func(ctx context.Context, _, _ string) (string, error) {
		fn, _ := plugin.ConfirmerFrom(ctx)
		ok, _ := fn(ctx, plugin.ConfirmRequest{Title: "执行命令"})
		if ok {
			return "执行了", nil
		}
		return "已放弃", nil
	}, nil)
	ctx := context.Background()

	// 没有等待中的确认时，/deny 要有明确说法
	c.Handle(ctx, Message{UserID: "u1", DedupID: "a", Text: "/deny"})
	if m := f.expect(t); !strings.Contains(m.text, "没有等待确认") {
		t.Fatalf("空确认时的回复异常: %s", m.text)
	}

	c.Handle(ctx, Message{UserID: "u1", DedupID: "b", Text: "干点事"})
	f.expect(t) // 确认请求
	c.Handle(ctx, Message{UserID: "u1", DedupID: "c", Text: "/deny"})
	if m := f.expect(t); !strings.Contains(m.text, "已拒绝") {
		t.Fatalf("应发拒绝回执: %s", m.text)
	}
	if m := f.expect(t); m.text != "已放弃" {
		t.Fatalf("拒绝后应走拒绝分支: %s", m.text)
	}
}

// 拿不到答复不等于得到许可：超时必须按拒绝处理。
func TestConfirmTimeoutDenies(t *testing.T) {
	denied := make(chan bool, 1)
	c, _ := newCore(t, func(ctx context.Context, _, _ string) (string, error) {
		fn, _ := plugin.ConfirmerFrom(ctx)
		ok, err := fn(ctx, plugin.ConfirmRequest{Title: "执行命令"})
		denied <- ok
		if err == nil {
			t.Error("超时应带错误")
		}
		return "结束", nil
	}, func(cfg *Config) { cfg.ConfirmTimeout = 200 * time.Millisecond })

	c.Handle(context.Background(), Message{UserID: "u1", Text: "干点事"})
	select {
	case ok := <-denied:
		if ok {
			t.Fatal("超时不该放行")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("确认没有超时返回")
	}
}

func TestQueueFullReplies(t *testing.T) {
	release := make(chan struct{})
	c, f := newCore(t, func(context.Context, string, string) (string, error) {
		<-release
		return "ok", nil
	}, func(cfg *Config) { cfg.QueueSize = 1 })
	t.Cleanup(func() { close(release) })

	ctx := context.Background()
	// 第一条进 worker 并卡住，第二条占满队列，第三条应被告知稍候
	for i := range 3 {
		c.Handle(ctx, Message{UserID: "u1", DedupID: fmt.Sprint(i), Text: "活儿"})
	}
	if m := f.expect(t); !strings.Contains(m.text, "请稍候再发") {
		t.Fatalf("队列满时应告知稍候: %s", m.text)
	}
}

func TestSessionForRebuildsMissing(t *testing.T) {
	c, _ := newCore(t, noopTurn, nil)
	if err := c.Bind("u1", "gone-1"); err != nil { // 不以 sess- 开头 = 已被删除
		t.Fatal(err)
	}
	sid, err := c.SessionFor("u1")
	if err != nil {
		t.Fatal(err)
	}
	if sid == "gone-1" {
		t.Fatal("会话已不存在时应重建")
	}
	if got := c.BoundSession("u1"); got != sid {
		t.Fatalf("重建后应落映射: %q vs %q", got, sid)
	}
}

func TestUsersForAndBindingPersists(t *testing.T) {
	dir := t.TempDir()
	f := newFakeSender()
	cfg := Config{
		PluginName: "test_bot", Sender: f, StateDir: dir,
		Allow: func(string) bool { return true }, RunTurn: noopTurn,
		NewSession: func() (string, error) { return "sess-1", nil },
		Sessions:   fakeSessions{},
	}
	c1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c1.Bind("u1", "sess-x"); err != nil {
		t.Fatal(err)
	}
	if err := c1.Bind("u2", "sess-x"); err != nil {
		t.Fatal(err)
	}
	users := c1.UsersFor("sess-x")
	if len(users) != 2 {
		t.Fatalf("应反查出两个用户: %v", users)
	}

	// 重启后同一用户继续原会话
	c2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.BoundSession("u1"); got != "sess-x" {
		t.Fatalf("绑定应持久化: %q", got)
	}
}

func TestTurnNotesForwarding(t *testing.T) {
	c, f := newCore(t, func(ctx context.Context, _, _ string) (string, error) {
		fn := plugin.TurnNotesFrom(ctx)
		if fn == nil {
			return "", fmt.Errorf("没有安装过程通知")
		}
		fn(plugin.TurnNote{Kind: plugin.NoteThinking, Text: "想了想"})
		fn(plugin.TurnNote{Kind: plugin.NoteToolCalls, Tools: []string{"read_file"}})
		return "结果", nil
	}, func(cfg *Config) { cfg.ShowThinking, cfg.ShowTools = true, true })

	c.Handle(context.Background(), Message{UserID: "u1", Text: "干活"})
	if m := f.expect(t); !strings.Contains(m.text, "🧠 思考过程") || !strings.Contains(m.text, "想了想") {
		t.Fatalf("思考链未转发: %s", m.text)
	}
	if m := f.expect(t); !strings.Contains(m.text, "🔧 调用工具") || !strings.Contains(m.text, "read_file") {
		t.Fatalf("工具调用未转发: %s", m.text)
	}
	if m := f.expect(t); m.text != "结果" {
		t.Fatalf("最终回复应排在过程之后: %s", m.text)
	}
}

// 两个开关都关时不安装回调：核心据此完全不产生通知开销。
func TestTurnNotesNotInstalledWhenDisabled(t *testing.T) {
	c, f := newCore(t, func(ctx context.Context, _, _ string) (string, error) {
		if plugin.TurnNotesFrom(ctx) != nil {
			return "", fmt.Errorf("不该安装过程通知")
		}
		return "结果", nil
	}, nil)
	c.Handle(context.Background(), Message{UserID: "u1", Text: "干活"})
	if m := f.expect(t); m.text != "结果" {
		t.Fatalf("回复异常: %s", m.text)
	}
}

func TestTypingHook(t *testing.T) {
	var states []bool
	var mu sync.Mutex
	c, f := newCore(t, noopTurn, func(cfg *Config) {
		cfg.Typing = func(_ context.Context, _ Message, active bool) {
			mu.Lock()
			states = append(states, active)
			mu.Unlock()
		}
	})
	c.Handle(context.Background(), Message{UserID: "u1", Text: "你好"})
	f.expect(t)
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 2 || !states[0] || states[1] {
		t.Fatalf("应在轮次前后各调一次（true 然后 false）: %v", states)
	}
}

func TestOnAcceptedRunsOnlyForAllowed(t *testing.T) {
	var got []string
	var mu sync.Mutex
	c, _ := newCore(t, noopTurn, func(cfg *Config) {
		cfg.OnAccepted = func(m Message) {
			mu.Lock()
			got = append(got, m.UserID)
			mu.Unlock()
		}
	})
	ctx := context.Background()
	c.Handle(ctx, Message{UserID: "stranger", DedupID: "a", Text: "你好"})
	c.Handle(ctx, Message{UserID: "u1", DedupID: "b", Text: "你好"})
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "u1" {
		t.Fatalf("只有放行的用户才该触发记账: %v", got)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	c, _ := newCore(t, noopTurn, nil)
	c.Stop()
	c.Stop() // 重复调用不该 panic 或卡住
	if c.Running() {
		t.Fatal("Stop 后不该仍在运行")
	}
}

func TestDeduper(t *testing.T) {
	d := NewDeduper()
	if d.IsDuplicate("a") {
		t.Fatal("首次不算重复")
	}
	if !d.IsDuplicate("a") {
		t.Fatal("窗口内应判重")
	}
	d.seen["b"] = time.Now().Add(-10 * time.Minute)
	if d.IsDuplicate("b") {
		t.Fatal("超窗后不算重复")
	}
	if d.IsDuplicate("") {
		t.Fatal("空 ID 不该算重复")
	}
}

// PushNotice 的过滤矩阵：开关、共享域、非 IM 来源、已绑定用户，全满足才推；
// 推送走 Push 通道，不做「自动接入唯一用户」。
func TestPushNotice(t *testing.T) {
	pushed := make(chan string, 8)
	c, _ := newCore(t, noopTurn, func(cfg *Config) {
		cfg.PushNotices = true
		cfg.Push = func(_ context.Context, userID, text string) bool {
			pushed <- userID + "|" + text
			return true
		}
	})
	if err := c.Bind("u1", "sess-1"); err != nil {
		t.Fatal(err)
	}

	expectPushed := func(want string) {
		t.Helper()
		select {
		case got := <-pushed:
			if got != want {
				t.Fatalf("推送内容不对：%q，期望 %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("等待注记推送超时")
		}
	}
	expectQuiet := func() {
		t.Helper()
		select {
		case got := <-pushed:
			t.Fatalf("不该有推送，却收到 %q", got)
		case <-time.After(150 * time.Millisecond):
		}
	}

	// 共享域、非 IM 来源、有绑定：推
	c.PushNotice(plugin.NoticeEvent{SessionID: "sess-1", Origin: "memory", Text: "🧠 记忆提炼：新增「事实/A」"})
	expectPushed("u1|🧠 记忆提炼：新增「事实/A」")

	// 带可见域标签：不推（归属拿不准，宁可少推）
	c.PushNotice(plugin.NoticeEvent{SessionID: "sess-1", Origin: "memory", Tag: "inner", Text: "里侧注记"})
	expectQuiet()

	// IM 通道自己发的注记（转投失败等）：不推。生产里各通道在 New() 里 Declare，
	// 测试的 Core 没走那条路，这里补一次声明（幂等）
	Declare("test_bot", "测试通道")
	c.PushNotice(plugin.NoticeEvent{SessionID: "sess-1", Origin: "test_bot", Text: "转投失败"})
	expectQuiet()

	// 来源为空（前台轮次写下的，如失败转译的报错原文）：不推
	c.PushNotice(plugin.NoticeEvent{SessionID: "sess-1", Text: "本轮模型调用失败，原始错误：401"})
	expectQuiet()

	// 没有用户绑定的会话：不推，也不自动接入
	c.PushNotice(plugin.NoticeEvent{SessionID: "sess-2", Origin: "memory", Text: "无人绑定"})
	expectQuiet()
}

// 开关关着（默认）时一律不推。
func TestPushNoticeDisabledByDefault(t *testing.T) {
	pushed := make(chan string, 1)
	c, _ := newCore(t, noopTurn, func(cfg *Config) {
		cfg.Push = func(_ context.Context, userID, text string) bool {
			pushed <- userID
			return true
		}
	})
	if err := c.Bind("u1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	c.PushNotice(plugin.NoticeEvent{SessionID: "sess-1", Origin: "memory", Text: "有内容"})
	select {
	case <-pushed:
		t.Fatal("开关默认关，不该推送")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestHandlePureImageMessage(t *testing.T) {
	var got []llm.ImagePart
	c, f := newCore(t, func(context.Context, string, string) (string, error) {
		t.Fatal("image-only message should use RunTurnImages")
		return "", nil
	}, func(cfg *Config) {
		cfg.RunTurnImages = func(_ context.Context, _, _ string, images []llm.ImagePart) (string, error) {
			got = images
			return "看到了", nil
		}
	})
	c.Handle(context.Background(), Message{UserID: "u1", Images: []llm.ImagePart{{MediaType: "image/png"}}})
	if m := f.expect(t); !strings.Contains(m.text, "看到了") {
		t.Fatalf("reply = %+v", m)
	}
	if len(got) != 1 {
		t.Fatalf("images = %+v", got)
	}
}

func TestHandleSendsAttachmentNoteWithText(t *testing.T) {
	c, f := newCore(t, func(context.Context, string, string) (string, error) {
		return "文字回复", nil
	}, nil)
	c.Handle(context.Background(), Message{UserID: "u1", Text: "带图的文字", Note: "图片下载失败"})
	first := f.expect(t)
	if first.text != "图片下载失败" {
		t.Fatalf("first reply = %+v", first)
	}
	second := f.expect(t)
	if second.text != "文字回复" {
		t.Fatalf("second reply = %+v", second)
	}
}
