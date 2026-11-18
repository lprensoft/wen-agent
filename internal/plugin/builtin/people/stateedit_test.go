package people

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

func editReq(op, row string, values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Row: row, Op: op, Values: values}
}

func TestEditPersonUpdatesFields(t *testing.T) {
	p := newTestPlugin(t)
	now := stateNow()
	if _, err := p.storeFor("").Upsert(Update{
		Name: "林舟", Relation: strPtr("大学同学"), Profile: strPtr("爱做菜"),
	}, now, Limits{MaxPeople: 5}); err != nil {
		t.Fatal(err)
	}

	err := p.EditState(editReq(stateOpEdit, "林舟", map[string]string{
		"relation": "同事", "closeness": "亲近", "lately": "在闹离职",
	}))
	if err != nil {
		t.Fatalf("EditState = %v", err)
	}
	ps, err := p.storeFor("").List()
	if err != nil {
		t.Fatal(err)
	}
	if ps[0].Relation != "同事" || ps[0].Closeness != "亲近" || ps[0].Lately != "在闹离职" {
		t.Fatalf("改动没有落盘：%+v", ps[0])
	}
}

// 只改关系/设定时，过期近况不能因为表单原样带回而被当作「刚听说」。
func TestUnrelatedEditKeepsLatelyTime(t *testing.T) {
	p := newTestPlugin(t)
	old := stateNow().Add(-40 * 24 * time.Hour)
	if _, err := p.storeFor("").Upsert(Update{
		Name: "林舟", Relation: strPtr("大学同学"), Lately: strPtr("在闹离职"),
	}, old, Limits{MaxPeople: 5}); err != nil {
		t.Fatal(err)
	}
	before, _ := p.storeFor("").List()

	if err := p.EditState(plugin.StateEditRequest{
		Lang: "zh", Domain: "", Section: stateSection, Row: "林舟", Op: stateOpEdit,
		Values: map[string]string{
			"relation": "同事", "profile": "", "closeness": "熟", "lately": "在闹离职",
		},
		Originals: map[string]string{
			"relation": "大学同学", "profile": "", "closeness": "熟", "lately": "在闹离职",
		},
	}); err != nil {
		t.Fatalf("只改关系 = %v", err)
	}
	after, _ := p.storeFor("").List()
	if after[0].Relation != "同事" {
		t.Fatalf("关系没改到：%+v", after[0])
	}
	if !after[0].LatelyAt.Equal(before[0].LatelyAt) {
		t.Errorf("没动的近况被刷新了：%v → %v", before[0].LatelyAt, after[0].LatelyAt)
	}
}

func TestAddAndDeletePerson(t *testing.T) {
	p := newTestPlugin(t)
	if err := p.EditState(editReq(stateOpAdd, "", map[string]string{
		"name": "周宁", "relation": "同事", "closeness": "熟", "profile": "坐对桌",
	})); err != nil {
		t.Fatalf("新增 = %v", err)
	}
	ps, _ := p.storeFor("").List()
	if len(ps) != 1 || ps[0].Name != "周宁" || ps[0].Closeness != "熟" {
		t.Fatalf("新增后 = %+v", ps)
	}

	if err := p.EditState(editReq(stateOpDelete, "周宁", nil)); err != nil {
		t.Fatalf("删除 = %v", err)
	}
	if ps, _ := p.storeFor("").List(); len(ps) != 0 {
		t.Fatalf("删除后 = %+v", ps)
	}
}

func TestEditRejectsUnknownTarget(t *testing.T) {
	p := newTestPlugin(t)
	if err := p.EditState(editReq(stateOpDelete, "不存在", nil)); err == nil {
		t.Error("没有这个人时应拒绝")
	}
	req := editReq(stateOpAdd, "", map[string]string{"name": "周宁", "relation": "同事"})
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
}

func TestAddRejectsBadInput(t *testing.T) {
	p := newTestPlugin(t)
	if err := p.EditState(editReq(stateOpAdd, "", map[string]string{"relation": "同事"})); err == nil {
		t.Error("空名字应拒绝")
	}
	if err := p.EditState(editReq(stateOpAdd, "", map[string]string{"name": "你", "relation": "同事"})); err == nil {
		t.Error("指代对方本人的名字应拒绝")
	}
	if err := p.EditState(editReq(stateOpAdd, "", map[string]string{"name": "周宁", "relation": "同事"})); err != nil {
		t.Fatalf("第一次新增 = %v", err)
	}
	if err := p.EditState(editReq(stateOpAdd, "", map[string]string{"name": "周宁", "relation": "朋友"})); err == nil {
		t.Error("同名应拒绝")
	}
}
