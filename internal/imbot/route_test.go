package imbot

import (
	"context"
	"strings"
	"testing"
	"time"
)

// resetRegistry 清空包级的通道注册表与路由。它们是进程级的，用例之间必须互不影响。
func resetRegistry(t *testing.T) {
	t.Helper()
	clear := func() {
		regMu.Lock()
		declared, liveCores, router, anchor = nil, map[string]*Core{}, nil, ""
		regMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// pushRecorder 记下主动推送，并可以被设成「推不出去」。
type pushRecorder struct {
	ch     chan sentMsg
	broken bool
}

func newPushRecorder() *pushRecorder { return &pushRecorder{ch: make(chan sentMsg, 16)} }

func (p *pushRecorder) push(_ context.Context, userID, text string) bool {
	if p.broken {
		return false
	}
	select {
	case p.ch <- sentMsg{userID: userID, text: text}:
	default:
	}
	return true
}

func (p *pushRecorder) expect(t *testing.T) sentMsg {
	t.Helper()
	select {
	case m := <-p.ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("等待转投消息超时")
		return sentMsg{}
	}
}

func (p *pushRecorder) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-p.ch:
		t.Fatalf("不该有转投消息，却收到 %+v", m)
	case <-time.After(d):
	}
}

// newNamedCore 造一条已启动的具名通道。
func newNamedCore(t *testing.T, name string) (*Core, *fakeSender, *pushRecorder) {
	t.Helper()
	rec := newPushRecorder()
	c, f := newCore(t, noopTurn, func(cfg *Config) {
		cfg.PluginName = name
		cfg.Push = rec.push
		cfg.Allow = func(string) bool { return true }
	})
	return c, f, rec
}

func TestDeclareIsIdempotentAndOrdered(t *testing.T) {
	resetRegistry(t)
	Declare("qq_bot", "QQ")
	Declare("wechat_bot", "微信")
	Declare("qq_bot", "别的名字") // 重复声明应被忽略
	Declare("", "空名字")        // 空名字不进表

	got := Channels()
	if len(got) != 2 {
		t.Fatalf("应有 2 条通道，得到 %d 条：%+v", len(got), got)
	}
	if got[0].Name != "qq_bot" || got[0].Label != "QQ" {
		t.Errorf("第一条应是首次声明的 QQ，得到 %+v", got[0])
	}
	if got[1].Name != "wechat_bot" {
		t.Errorf("顺序应按声明顺序，得到 %+v", got)
	}
	if !IsChannel("qq_bot") || IsChannel("heartbeat") || IsChannel("") {
		t.Error("IsChannel 只应认已声明的通道名")
	}
}

func TestChannelLiveFollowsStartStop(t *testing.T) {
	resetRegistry(t)
	Declare("a_bot", "A")
	Declare("b_bot", "B")

	live := func() map[string]bool {
		m := map[string]bool{}
		for _, ch := range Channels() {
			m[ch.Name] = ch.Live
		}
		return m
	}
	if l := live(); l["a_bot"] || l["b_bot"] {
		t.Fatalf("尚未启动时都该是未启用：%+v", l)
	}

	a, _, _ := newNamedCore(t, "a_bot")
	if l := live(); !l["a_bot"] || l["b_bot"] {
		t.Fatalf("只有 a_bot 该是启用状态：%+v", l)
	}
	a.Stop()
	if l := live(); l["a_bot"] {
		t.Fatalf("Stop 之后应注销：%+v", l)
	}
}

func TestRouterInstallAndClear(t *testing.T) {
	resetRegistry(t)
	if Target("sess-1") != "" || !ServedBy("a_bot", "sess-1") {
		t.Fatal("未安装路由时应不指定目标，且任何通道都算负责")
	}

	SetRouter(func(sid string) string {
		if sid == "sess-inner" {
			return "b_bot"
		}
		return ""
	})
	if got := Target("sess-inner"); got != "b_bot" {
		t.Errorf("目标应为 b_bot，得到 %q", got)
	}
	if ServedBy("a_bot", "sess-inner") {
		t.Error("目标是 b_bot 时 a_bot 不该认为自己负责")
	}
	if !ServedBy("b_bot", "sess-inner") {
		t.Error("目标通道自己应认为负责")
	}
	if !ServedBy("a_bot", "sess-other") {
		t.Error("路由不指定目标时任何通道都算负责")
	}

	SetRouter(nil)
	if Target("sess-inner") != "" {
		t.Error("SetRouter(nil) 应清除路由")
	}
}

func TestDeliverNeedsALiveChannel(t *testing.T) {
	resetRegistry(t)
	Declare("b_bot", "B")
	if Deliver(context.Background(), "", "b_bot", "sess-1", "话") {
		t.Error("通道声明了但没启动，不该报投递成功")
	}
	if Deliver(context.Background(), "", "不存在", "sess-1", "话") {
		t.Error("未知通道不该报投递成功")
	}
}

func TestDeliverUsesUsersAlreadyBoundToSession(t *testing.T) {
	resetRegistry(t)
	b, _, rec := newNamedCore(t, "b_bot")
	if err := b.Bind("v1", "sess-9"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := b.Bind("v2", "sess-other"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if !Deliver(context.Background(), "", "b_bot", "sess-9", "里人格的话") {
		t.Fatal("已有用户绑在该会话上，应投递成功")
	}
	got := rec.expect(t)
	if got.userID != "v1" || got.text != "里人格的话" {
		t.Errorf("应只投给绑在该会话上的 v1，得到 %+v", got)
	}
	rec.expectNone(t, 200*time.Millisecond)
}

func TestDeliverAdoptsTheSoleKnownUser(t *testing.T) {
	resetRegistry(t)
	b, _, rec := newNamedCore(t, "b_bot")
	// 这位用户此前在自己的会话里聊天，本通道还没有人绑到 sess-9 上
	if err := b.Bind("v1", "sess-old"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if !Deliver(context.Background(), "", "b_bot", "sess-9", "接手了") {
		t.Fatal("只有一位已知用户时应把他接到目标会话并投递")
	}
	if got := rec.expect(t); got.userID != "v1" {
		t.Errorf("应投给 v1，得到 %+v", got)
	}
	if sid := b.BoundSession("v1"); sid != "sess-9" {
		t.Errorf("应换绑到 sess-9，实际 %q", sid)
	}

	// 换绑要落盘，否则重启后两条通道又各回各的会话
	reloaded, err := loadBinding(b.cfg.StateDir)
	if err != nil {
		t.Fatalf("重新加载映射: %v", err)
	}
	if sid := reloaded.Get("v1"); sid != "sess-9" {
		t.Errorf("换绑未落盘，重新加载得到 %q", sid)
	}
}

func TestDeliverRefusesWhenRecipientIsAmbiguous(t *testing.T) {
	resetRegistry(t)
	b, _, rec := newNamedCore(t, "b_bot")

	// 一个用户都没有：没人可投
	if Deliver(context.Background(), "", "b_bot", "sess-9", "话") {
		t.Error("该通道还没有任何用户说过话，不该报投递成功")
	}

	// 已知用户不止一个：宁可不送达，也不把一侧人格的话广播给所有人
	if err := b.Bind("v1", "sess-a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := b.Bind("v2", "sess-b"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if Deliver(context.Background(), "", "b_bot", "sess-9", "话") {
		t.Error("已知用户不止一个时不该自动选人投递")
	}
	rec.expectNone(t, 200*time.Millisecond)
}

func TestDeliverNeedsPushCapability(t *testing.T) {
	resetRegistry(t)
	c, _ := newCore(t, noopTurn, func(cfg *Config) {
		cfg.PluginName = "b_bot"
		cfg.Push = nil // 通道没实现主动推送
	})
	if err := c.Bind("v1", "sess-9"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if Deliver(context.Background(), "", "b_bot", "sess-9", "话") {
		t.Error("通道不支持主动推送时不该报投递成功")
	}
}

// 端到端：一轮对话的回复该落在目标通道上，来源通道一声不吭。
func TestChatReplyGoesToTargetChannel(t *testing.T) {
	resetRegistry(t)
	Declare("a_bot", "A")
	Declare("b_bot", "B")
	a, senderA, _ := newNamedCore(t, "a_bot")
	b, _, recB := newNamedCore(t, "b_bot")
	if err := b.Bind("v1", "sess-old"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	SetRouter(func(string) string { return "b_bot" })

	a.Handle(context.Background(), Message{UserID: "u1", DedupID: "m1", Text: "你好", ReplyTo: "r1"})

	if got := recB.expect(t); got.text != "收到：你好" {
		t.Errorf("目标通道应收到回复，得到 %+v", got)
	}
	senderA.expectNone(t, 300*time.Millisecond)
}

// 转投失败时不回落到来源通道——那等于把另一侧人格的话直接摆过去。
func TestFailedDeliveryDropsAndNotices(t *testing.T) {
	resetRegistry(t)
	Declare("a_bot", "A")
	Declare("b_bot", "B")

	notices := make(chan string, 4)
	a, senderA := newCore(t, noopTurn, func(cfg *Config) {
		cfg.PluginName = "a_bot"
		cfg.Notice = func(_ context.Context, sid, text string) error {
			notices <- sid + "|" + text
			return nil
		}
	})
	b, _, recB := newNamedCore(t, "b_bot")
	recB.broken = true // 比如微信那边缺 context_token
	if err := b.Bind("v1", "sess-old"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	SetRouter(func(string) string { return "b_bot" })

	a.Handle(context.Background(), Message{UserID: "u1", DedupID: "m1", Text: "你好"})

	select {
	case n := <-notices:
		if !strings.Contains(n, "b_bot") {
			t.Errorf("注记应说明该发往哪条通道，得到 %q", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待会话注记超时")
	}
	senderA.expectNone(t, 300*time.Millisecond)
}

// 没有路由时一切照旧：这是「关掉开关行为不变」的直接证据。
func TestWithoutRouterReplyGoesBackTheSameWay(t *testing.T) {
	resetRegistry(t)
	a, senderA, _ := newNamedCore(t, "a_bot")

	a.Handle(context.Background(), Message{UserID: "u1", DedupID: "m1", Text: "你好", ReplyTo: "r1"})

	got := senderA.expect(t)
	if got.userID != "u1" || got.text != "收到：你好" || got.replyTo != "r1" {
		t.Errorf("应原路回复并带上回复凭据，得到 %+v", got)
	}
}

// 路由指向自己时也走原路：不绕一圈主动推送，回复凭据（被动回复窗口）才不会丢。
func TestRouterPointingAtSelfKeepsThePassiveReply(t *testing.T) {
	resetRegistry(t)
	a, senderA, recA := newNamedCore(t, "a_bot")
	SetRouter(func(string) string { return "a_bot" })

	a.Handle(context.Background(), Message{UserID: "u1", DedupID: "m1", Text: "你好", ReplyTo: "r1"})

	if got := senderA.expect(t); got.replyTo != "r1" {
		t.Errorf("应保留回复凭据，得到 %+v", got)
	}
	recA.expectNone(t, 200*time.Millisecond)
}
