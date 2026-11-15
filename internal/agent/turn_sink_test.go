package agent

import (
	"context"
	"testing"
	"time"

	"wen/internal/plugin"
)

// 一轮结束要留下两样东西：meta 上的最近消息时间（界面用来判未读），以及推给
// 服务端的事件。机器自发的轮次也算——那正是用户需要被提醒的那些。
func TestTurnEndRecordsMessageAndNotifiesSink(t *testing.T) {
	store, id := newTestSession(t)
	mp := &mockProvider{turns: []mockTurn{{content: "回答"}}}
	ag := New(mp, newTestManager(t), store, Options{Model: "m"})

	var got []string
	var gotEv plugin.TurnEndEvent
	ag.SetTurnSink(func(sid string, ev plugin.TurnEndEvent) {
		got = append(got, sid)
		gotEv = ev
	})

	if _, err := ag.RunTurn(plugin.WithTurnOrigin(context.Background(), "scheduler"), id, "【心跳】看看在不在"); err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0] != id {
		t.Fatalf("sink 收到 = %v，期望 [%s]", got, id)
	}
	if gotEv.Origin != "scheduler" || gotEv.Interactive || gotEv.FinalText != "回答" {
		t.Errorf("事件字段不符: %+v", gotEv)
	}

	meta, _, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastMessageAt == nil {
		t.Fatal("LastMessageAt 没有被记下")
	}
	if meta.LastMessageAt.Before(gotEv.StartedAt) || meta.LastMessageAt.After(time.Now()) {
		t.Errorf("LastMessageAt = %v，超出本轮范围", meta.LastMessageAt)
	}
	// 机器轮次不该把自己算成「真人最近活跃」，否则以最近活跃为落点的后台功能会续命。
	if meta.LastActiveAt != nil {
		t.Errorf("非交互轮次不该更新 LastActiveAt: %v", meta.LastActiveAt)
	}
}

// 注记也是界面上的新内容：后台任务写了一条，会话列表就该亮起来。
func TestAppendNoticeRecordsMessageTime(t *testing.T) {
	store, id := newTestSession(t)
	ag := New(&mockProvider{}, newTestManager(t), store, Options{Model: "m"})

	if err := ag.AppendNotice(context.Background(), id, "记忆提炼完成"); err != nil {
		t.Fatal(err)
	}
	meta, _, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastMessageAt == nil {
		t.Fatal("注记没有更新 LastMessageAt")
	}
}

// 「保持安静」的一轮：模型给空回复就等于什么都没说。它仍要通知插件（心跳靠
// OnTurnEnd 记账），但不该标未读、不该弹通知。
func TestSilentTurnDoesNotMarkUnread(t *testing.T) {
	store, id := newTestSession(t)
	ob := &observerPlugin{}
	mp := &mockProvider{turns: []mockTurn{{content: "   "}}}
	ag := New(mp, newTestManager(t, ob), store, Options{Model: "m"})

	called := false
	ag.SetTurnSink(func(string, plugin.TurnEndEvent) { called = true })
	if _, err := ag.RunTurn(context.Background(), id, "【心跳】"); err != nil {
		t.Fatal(err)
	}

	if called {
		t.Error("空回复不该推 turn 事件")
	}
	meta, _, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastMessageAt != nil {
		t.Errorf("空回复不该更新 LastMessageAt: %v", meta.LastMessageAt)
	}
	if len(ob.evs) != 1 {
		t.Errorf("插件仍应收到一次轮次结束，实际 %d", len(ob.evs))
	}
}
