package imbot

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// newGroupCore 造一条具名通道，会话 ID 带通道名，便于分辨一个会话是谁建的。
// newCore 的默认 NewSession 各自从 1 开始编号，两条通道会造出同名会话，
// 「是否落在同一个会话上」就测不出来了。
func newGroupCore(t *testing.T, name string) (*Core, *pushRecorder) {
	t.Helper()
	rec := newPushRecorder()
	var seq atomic.Int64
	c, _ := newCore(t, noopTurn, func(cfg *Config) {
		cfg.PluginName = name
		cfg.Push = rec.push
		cfg.Allow = func(string) bool { return true }
		cfg.NewSession = func() (string, error) {
			return fmt.Sprintf("sess-%s-%d", name, seq.Add(1)), nil
		}
	})
	return c, rec
}

// 装了分通道路由时，后来的通道加入已经在用的那个会话，而不是各建各的。
// 这正是「表→里→表 接不上」的根因：两个人格待在两个会话里。
func TestGroupAnchorSharesOneSession(t *testing.T) {
	resetRegistry(t)
	SetRouter(func(string) string { return "" })
	a, _ := newGroupCore(t, "a_bot")
	b, _ := newGroupCore(t, "b_bot")

	first, err := a.SessionFor("u1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.SessionFor("u2")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("两条通道应落在同一个会话上：a=%s b=%s", first, second)
	}
	if got := b.BoundSession("u2"); got != first {
		t.Errorf("加入时该把绑定一并写过去，得到 %q", got)
	}
}

// 没装路由时行为一字不变：各通道各有各的会话。
func TestWithoutRouterSessionsStayPerChannel(t *testing.T) {
	resetRegistry(t)
	a, _ := newGroupCore(t, "a_bot")
	b, _ := newGroupCore(t, "b_bot")

	first, _ := a.SessionFor("u1")
	second, _ := b.SessionFor("u2")
	if first == second {
		t.Errorf("未分通道时不该共用会话：%s", first)
	}
}

// 锚指向的会话已被删除时退回常规路径，不能把人绑到一个不存在的会话上。
func TestGroupAnchorIgnoresMissingSession(t *testing.T) {
	resetRegistry(t)
	SetRouter(func(string) string { return "" })
	b, _ := newGroupCore(t, "b_bot")

	setGroupAnchor("已经删掉的会话") // fakeSessions 只认 sess- 前缀
	sid, err := b.SessionFor("u2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sid, "sess-b_bot-") {
		t.Errorf("锚失效时应新建自己的会话，得到 %q", sid)
	}
}

// /new 移动整组：另一条通道在它的下一条消息上跟到新会话。
// 只搬自己的话，两个人格当场分家，正是这次要修的事。
func TestNewMovesWholeGroup(t *testing.T) {
	resetRegistry(t)
	SetRouter(func(string) string { return "" })
	a, _ := newGroupCore(t, "a_bot")
	b, _ := newGroupCore(t, "b_bot")

	first, _ := a.SessionFor("u1")
	if _, err := b.SessionFor("u2"); err != nil {
		t.Fatal(err)
	}
	a.cmdNew(context.Background(), Message{UserID: "u1"})

	next := a.BoundSession("u1")
	if next == "" || next == first {
		t.Fatalf("/new 应换一个会话，得到 %q", next)
	}
	sid, _ := b.SessionFor("u2")
	if sid != next {
		t.Errorf("另一条通道应跟到新会话 %s，得到 %s", next, sid)
	}
}

// 自动接管改写了一个已有绑定时要留痕。此前它完全无声，用户看到的只是
// 「切回来之后接不上了」。
func TestAdoptionFromAnotherSessionLeavesNotice(t *testing.T) {
	resetRegistry(t)
	SetRouter(func(string) string { return "b_bot" })
	notices := make(chan string, 4)
	rec := newPushRecorder()
	b, _ := newCore(t, noopTurn, func(cfg *Config) {
		cfg.PluginName = "b_bot"
		cfg.Push = rec.push
		cfg.Allow = func(string) bool { return true }
		cfg.Notice = func(_ context.Context, _, text string) error {
			notices <- text
			return nil
		}
	})
	if err := b.Bind("u2", "sess-old"); err != nil {
		t.Fatal(err)
	}

	if !Deliver(context.Background(), "", "b_bot", "sess-new", "一段话") {
		t.Fatal("应投递成功")
	}
	rec.expect(t)

	select {
	case n := <-notices:
		if !strings.Contains(n, "sess-old") || !strings.Contains(n, "b_bot") {
			t.Errorf("注记应说清是谁、从哪个会话搬来的：%s", n)
		}
	default:
		t.Error("改绑应留一条会话注记")
	}
	if got := b.BoundSession("u2"); got != "sess-new" {
		t.Errorf("接管后绑定应指向新会话，得到 %q", got)
	}
}
