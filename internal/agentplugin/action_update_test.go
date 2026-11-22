package agentplugin

import (
	"context"
	"testing"
	"time"

	"wen/internal/plugin"
)

func waitExternalAction(t *testing.T, p *Plugin, key string) plugin.ActionState {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.ActionState(key)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != plugin.ActionPending {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("外源插件操作一直没结束")
	return plugin.ActionState{}
}

func TestSkillOnlyBundleUpdateActions(t *testing.T) {
	f := newStageFixture(t)
	b, err := Load("", f.store.Path("pdf-tools"))
	if err != nil {
		t.Fatal(err)
	}
	p := NewPlugin("", b, f.store)
	actions := p.Actions()
	if len(actions) != 2 {
		t.Fatalf("该有检查与更新两个操作：%+v", actions)
	}
	var updateAction plugin.ActionDef
	for _, action := range actions {
		if action.Key == actionStageUpdate {
			updateAction = action
		}
	}
	if updateAction.Token != f.candidate.Token {
		t.Fatalf("更新按钮没有绑定候选 token：%+v", updateAction)
	}

	ctx := plugin.WithActionToken(context.Background(), updateAction.Token)
	if err := p.StartAction(ctx, actionStageUpdate); err != nil {
		t.Fatal(err)
	}
	if st := waitExternalAction(t, p, actionStageUpdate); st.Status != plugin.ActionDone {
		t.Fatalf("暂存操作失败：%+v", st)
	}
	if pending, _ := f.store.Pending("pdf-tools"); pending == nil {
		t.Fatal("操作没有暂存更新")
	}
	actions = p.Actions()
	if len(actions) != 1 || actions[0].Key != actionCancelUpdate {
		t.Fatalf("待更新时该只显示取消：%+v", actions)
	}
	if err := p.StartAction(context.Background(), actionCancelUpdate); err != nil {
		t.Fatal(err)
	}
	if st := waitExternalAction(t, p, actionCancelUpdate); st.Status != plugin.ActionDone {
		t.Fatalf("取消失败：%+v", st)
	}
}

func TestCancelActionReturnsImmediatelyWhileUpdateLockHeld(t *testing.T) {
	f := newStageFixture(t)
	if _, err := f.store.StageUpdate(context.Background(), "pdf-tools", f.candidate.Token); err != nil {
		t.Fatal(err)
	}
	b, _ := Load("", f.store.Path("pdf-tools"))
	p := NewPlugin("", b, f.store)
	release, err := f.store.acquireUpdateLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	returned := make(chan error, 1)
	go func() { returned <- p.StartAction(context.Background(), actionCancelUpdate) }()
	select {
	case err := <-returned:
		if err != nil {
			release()
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		release()
		t.Fatal("取消操作在 HTTP 调用里等待了文件锁")
	}
	st, _ := p.ActionState(actionCancelUpdate)
	if st.Status != plugin.ActionPending {
		release()
		t.Fatalf("锁被占用时取消应处于进行中：%+v", st)
	}
	release()
	if st := waitExternalAction(t, p, actionCancelUpdate); st.Status != plugin.ActionDone {
		t.Fatalf("释放锁后取消没有完成：%+v", st)
	}
}

func TestExternalActionsRejectConcurrencyAndKeepStatesSeparate(t *testing.T) {
	p := &Plugin{name: "ext_demo", bundle: &Bundle{Manifest: Manifest{Name: "demo"}}}
	seq, err := p.beginAction(actionCheckUpdate, plugin.ActionState{Status: plugin.ActionPending, Message: "checking"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.beginAction(actionRefresh, plugin.ActionState{Status: plugin.ActionPending}); err == nil {
		t.Fatal("并发操作没有被拒绝")
	}
	p.finishAction(actionCheckUpdate, seq, plugin.ActionState{Status: plugin.ActionDone, Message: "done"})
	check, _ := p.ActionState(actionCheckUpdate)
	refresh, _ := p.ActionState(actionRefresh)
	if check.Message != "done" || refresh.Status != "" {
		t.Fatalf("操作状态互相覆盖：%+v / %+v", check, refresh)
	}
	if _, err := p.ActionState("nope"); err == nil {
		t.Fatal("未知操作没有报错")
	}
}
