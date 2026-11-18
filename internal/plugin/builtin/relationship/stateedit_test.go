package relationship

import (
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func rptr(s string) *string { return &s }

func relEditReq(values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Op: stateOpEdit, Values: values}
}

func TestEditRelationshipFromState(t *testing.T) {
	p := newTestPlugin(t)
	if _, _, err := p.storeFor("").Apply(Update{
		Stage: rptr("朋友"), MyCall: rptr("小林"),
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := p.EditState(relEditReq(map[string]string{
		"stage": "恋人", "my_call": "宝贝", "their_call": "阿川",
		"recent": "在一起了", "their_now": "在出差",
		"bonds": "不吃香菜\n不提前任",
	})); err != nil {
		t.Fatalf("修改 = %v", err)
	}
	snap, ok, err := p.storeFor("").Load()
	if err != nil || !ok {
		t.Fatalf("Load = %v / %v", ok, err)
	}
	if snap.Stage != "恋人" || snap.MyCall != "宝贝" || snap.TheirCall != "阿川" {
		t.Fatalf("字段 = %+v", snap)
	}
	if len(snap.Bonds) != 2 || snap.Bonds[0] != "不吃香菜" {
		t.Fatalf("默契 = %+v", snap.Bonds)
	}
	// 清空一行即清掉那一条。
	if err := p.EditState(relEditReq(map[string]string{"bonds": ""})); err != nil {
		t.Fatalf("清空默契 = %v", err)
	}
	if snap, _, _ := p.storeFor("").Load(); len(snap.Bonds) != 0 {
		t.Errorf("清空后 = %+v", snap.Bonds)
	}
}

func TestRecentTimestampNotRefreshedByUnrelatedEdit(t *testing.T) {
	p := newTestPlugin(t)
	old := time.Now().Add(-72 * time.Hour)
	if _, _, err := p.storeFor("").Apply(Update{
		Stage: rptr("朋友"), Recent: rptr("一起看了电影"),
	}, old); err != nil {
		t.Fatal(err)
	}
	before, _, err := p.storeFor("").Load()
	if err != nil {
		t.Fatal(err)
	}

	// 只改称呼，表单里带回来的「最近变动」原样不变：时刻不能刷新。
	if err := p.EditState(relEditReq(map[string]string{
		"stage": "朋友", "my_call": "小心", "recent": "一起看了电影",
	})); err != nil {
		t.Fatalf("改称呼 = %v", err)
	}
	after, _, _ := p.storeFor("").Load()
	if !after.RecentAt.Equal(before.RecentAt) {
		t.Errorf("最近变动的时刻被无关修改刷新了：%v → %v", before.RecentAt, after.RecentAt)
	}

	// 内容真的变了才刷新时刻。
	if err := p.EditState(relEditReq(map[string]string{"recent": "在一起了"})); err != nil {
		t.Fatalf("改最近变动 = %v", err)
	}
	after, _, _ = p.storeFor("").Load()
	if !after.RecentAt.After(before.RecentAt) {
		t.Errorf("改了内容时刻没有更新：%v", after.RecentAt)
	}
}

func TestRelationshipEditRejectsBadInput(t *testing.T) {
	p := newTestPlugin(t)
	if err := p.EditState(relEditReq(map[string]string{"stage": strings.Repeat("字", 11)})); err == nil {
		t.Error("超长阶段应拒绝")
	}
	if err := p.EditState(relEditReq(map[string]string{"bonds": "1\n2\n3\n4\n5\n6"})); err == nil {
		t.Error("默契超过条数上限应拒绝")
	}
	req := relEditReq(map[string]string{"stage": "朋友"})
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
}
