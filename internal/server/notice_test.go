package server

import (
	"strings"
	"testing"
	"time"

	"wen/internal/llm"
	"wen/internal/session"
)

func testNotice(text string) session.StoredMessage {
	return session.StoredMessage{
		Message: llm.Message{Role: llm.RoleSystem, Content: text},
		Kind:    session.KindNotice,
		Origin:  "memory",
		TS:      time.Now(),
	}
}

func TestNoticeHubBroadcasts(t *testing.T) {
	h := newNoticeHub()
	a, closeA := h.subscribe()
	b, closeB := h.subscribe()
	defer closeA()
	defer closeB()

	h.publish("s1", testNotice("🧠 记忆提炼：新增「事实/某事」"))

	for i, ch := range []<-chan hubEvent{a, b} {
		select {
		case ev := <-ch:
			if ev.name != "notice" {
				t.Errorf("订阅者 %d 收到的事件名 = %q，期望 notice", i, ev.name)
			}
			if !strings.Contains(string(ev.data), "记忆提炼") || !strings.Contains(string(ev.data), `"session_id":"s1"`) {
				t.Errorf("订阅者 %d 收到的内容不对：%s", i, ev.data)
			}
		case <-time.After(time.Second):
			t.Errorf("订阅者 %d 没收到广播", i)
		}
	}
}

// 一轮结束也是一条事件，事件名不同：界面靠它标未读、弹通知。
func TestNoticeHubBroadcastsTurn(t *testing.T) {
	h := newNoticeHub()
	ch, unsubscribe := h.subscribe()
	defer unsubscribe()

	ts := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	h.publishTurn("s1", ts, false, "早点睡。\n我关灯了。")

	select {
	case ev := <-ch:
		if ev.name != "turn" {
			t.Fatalf("事件名 = %q，期望 turn", ev.name)
		}
		got := string(ev.data)
		for _, want := range []string{`"session_id":"s1"`, `"interactive":false`, "早点睡。 我关灯了。"} {
			if !strings.Contains(got, want) {
				t.Errorf("turn 事件缺少 %q：%s", want, got)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("没收到 turn 事件")
	}
}

func TestTurnPreview(t *testing.T) {
	if got := turnPreview("a\n\nb\tc"); got != "a b c" {
		t.Errorf("preview = %q，期望折叠空白", got)
	}
	long := strings.Repeat("字", 90)
	got := turnPreview(long)
	if n := len([]rune(got)); n != 81 {
		t.Errorf("截断后长度 = %d，期望 81（80 加省略号）", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断后应以省略号结尾：%q", got)
	}
}

func TestNoticeHubUnsubscribe(t *testing.T) {
	h := newNoticeHub()
	ch, unsubscribe := h.subscribe()
	unsubscribe()

	if _, ok := <-ch; ok {
		t.Error("退订后通道应关闭")
	}
	h.publish("s1", testNotice("退订之后不该再收到")) // 不该 panic（往已关闭的通道写）
	unsubscribe()                            // 重复退订也应安全
}

func TestNoticeHubNeverBlocksOnSlowSubscriber(t *testing.T) {
	h := newNoticeHub()
	_, cleanup := h.subscribe() // 订阅了但一直不读
	defer cleanup()

	// publish 跑在插件的后台 goroutine 上，被一个不读的浏览器卡住是不可接受的：
	// 缓冲满了就丢，内容已经落盘，刷新页面即可补齐
	done := make(chan struct{})
	go func() {
		for range noticeBuffer * 3 {
			h.publish("s1", testNotice("塞满缓冲"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("慢订阅者不该拖住广播")
	}
}
