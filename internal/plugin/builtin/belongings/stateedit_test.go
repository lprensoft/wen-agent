package belongings

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

func editReq(op, row string, values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Row: row, Op: op, Values: values}
}

func seed(t *testing.T, p *Plugin) {
	t.Helper()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	if _, err := p.storeFor("").Apply("冰箱",
		[]Change{{Name: "鸡蛋", Qty: 6, Note: "今天买的"}}, nil, now,
		Limits{MaxContainers: 10, MaxItemsPerContainer: 50}); err != nil {
		t.Fatal(err)
	}
}

func TestTakeItem(t *testing.T) {
	p := newTestPlugin(t)
	seed(t, p)

	if err := p.EditState(editReq(stateOpTake, itemID("冰箱", "鸡蛋"), map[string]string{"qty": "2"})); err != nil {
		t.Fatalf("取出 = %v", err)
	}
	cs, _ := p.storeFor("").List()
	if cs[0].Items[0].Qty != 4 {
		t.Fatalf("取出后数量 = %d，期望 4", cs[0].Items[0].Qty)
	}

	if err := p.EditState(editReq(stateOpTake, itemID("冰箱", "鸡蛋"), map[string]string{"qty": "0"})); err != nil {
		t.Fatalf("全部取出 = %v", err)
	}
	if cs, _ := p.storeFor("").List(); len(cs[0].Items) != 0 {
		t.Fatalf("全部取出后仍有物品：%+v", cs[0].Items)
	}
}

func TestTakeRejectsBadTarget(t *testing.T) {
	p := newTestPlugin(t)
	seed(t, p)
	if err := p.EditState(editReq(stateOpTake, itemID("冰箱", "牛奶"), map[string]string{"qty": "1"})); err == nil {
		t.Error("不存在的物品应拒绝")
	}
	if err := p.EditState(editReq(stateOpTake, itemID("冰箱", "鸡蛋"), map[string]string{"qty": "-1"})); err == nil {
		t.Error("负数应拒绝")
	}
	req := editReq(stateOpTake, itemID("冰箱", "鸡蛋"), map[string]string{"qty": "1"})
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
}

func TestAddItem(t *testing.T) {
	p := newTestPlugin(t)
	if err := p.EditState(editReq(stateOpAdd, "", map[string]string{
		"container": "衣柜", "name": "雨伞", "qty": "1", "note": "黑色的",
	})); err != nil {
		t.Fatalf("放入 = %v", err)
	}
	cs, _ := p.storeFor("").List()
	if len(cs) != 1 || cs[0].Name != "衣柜" || len(cs[0].Items) != 1 {
		t.Fatalf("放入后 = %+v", cs)
	}
	if cs[0].Items[0].Name != "雨伞" || cs[0].Items[0].Note != "黑色的" {
		t.Fatalf("物品 = %+v", cs[0].Items[0])
	}
	if err := p.EditState(editReq(stateOpAdd, "", map[string]string{"container": "衣柜", "name": " "})); err == nil {
		t.Error("空物品名应拒绝")
	}
}
