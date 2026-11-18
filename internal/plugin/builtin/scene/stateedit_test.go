package scene

import (
	"testing"

	"wen/internal/plugin"
)

func editReq(op, row string, values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Row: row, Op: op, Values: values}
}

func TestEditSceneDetail(t *testing.T) {
	p := newTestPlugin(t, nil)
	if _, err := p.storeFor("").Save(Scene{Name: "城南书店", Detail: "两层"}, true); err != nil {
		t.Fatal(err)
	}
	if err := p.EditState(editReq(stateOpEdit, "城南书店", map[string]string{"detail": "两层，靠窗有一排旧书"})); err != nil {
		t.Fatalf("改场景 = %v", err)
	}
	scenes, _ := p.storeFor("").List()
	if len(scenes) != 1 || scenes[0].Detail != "两层，靠窗有一排旧书" {
		t.Fatalf("场景 = %+v", scenes)
	}
	if err := p.EditState(editReq(stateOpEdit, "城南书店", map[string]string{"detail": " "})); err == nil {
		t.Error("空描述应拒绝")
	}
	if err := p.EditState(editReq(stateOpEdit, "不存在", map[string]string{"detail": "x"})); err == nil {
		t.Error("改不存在的场景应拒绝")
	}
}

func TestAddAndDeleteScene(t *testing.T) {
	p := newTestPlugin(t, nil)
	if err := p.EditState(editReq(stateOpAdd, "", map[string]string{"name": "家", "detail": "阳台朝南"})); err != nil {
		t.Fatalf("新增 = %v", err)
	}
	if err := p.EditState(editReq(stateOpAdd, "", map[string]string{"name": "家", "detail": "重复"})); err == nil {
		t.Error("同名场景应拒绝")
	}
	if err := p.EditState(editReq(stateOpDelete, "家", nil)); err != nil {
		t.Fatalf("删除 = %v", err)
	}
	if scenes, _ := p.storeFor("").List(); len(scenes) != 0 {
		t.Fatalf("删除后 = %+v", scenes)
	}
}

func TestSceneEditRejectsUnknownDomain(t *testing.T) {
	p := newTestPlugin(t, nil)
	req := editReq(stateOpAdd, "", map[string]string{"name": "家", "detail": "阳台朝南"})
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
	if err := p.EditState(editReq(stateOpAdd, "", map[string]string{"name": "家", "detail": "阳台朝南"})); err != nil {
		t.Fatalf("共享域应可写：%v", err)
	}
}
