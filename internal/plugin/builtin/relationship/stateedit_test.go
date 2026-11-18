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
