package agenda

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

func editReq(op, row string, values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Row: row, Op: op, Values: values}
}

func seedPlan(t *testing.T, p *Plugin, now time.Time) {
	t.Helper()
	day := p.snapshot().today(now)
	if err := p.storeFor("").SavePlan(Plan{Date: day.Format(dateLayout), PlannedAt: now, Items: []Item{
		{ID: "a1", Title: "整理提案", Start: "09:30", End: "10:30", Place: "书房", Status: statusPlanned},
		{ID: "a2", Title: "做晚饭", Start: "19:00", End: "20:00", Status: statusDone},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestEditPlanItemFromState(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	seedPlan(t, p, now)

	if err := p.EditState(editReq(stateOpEdit, "a1", map[string]string{
		"title": "整理新提案", "start": "10:00", "end": "11:00", "place": "客厅",
	})); err != nil {
		t.Fatalf("改项 = %v", err)
	}
	pl, err := p.storeFor("").LoadPlan()
	if err != nil {
		t.Fatal(err)
	}
	it := pl.item("a1")
	if it == nil || it.Title != "整理新提案" || it.Start != "10:00" || it.End != "11:00" || it.Place != "客厅" {
		t.Fatalf("改后 = %+v", it)
	}
}

func TestCancelPlanItemFromState(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	seedPlan(t, p, now)

	if err := p.EditState(editReq(stateOpCancel, "a1", nil)); err != nil {
		t.Fatalf("取消 = %v", err)
	}
	pl, _ := p.storeFor("").LoadPlan()
	if it := pl.item("a1"); it == nil || it.Status != statusCancelled || it.Outcome == "" {
		t.Fatalf("取消后 = %+v", it)
	}
	// 已经结束的项不能再取消。
	if err := p.EditState(editReq(stateOpCancel, "a2", nil)); err == nil {
		t.Error("已做完的项应拒绝")
	}
}

func TestPlanEditRejectsBadInput(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	seedPlan(t, p, now)

	if err := p.EditState(editReq(stateOpEdit, "a1", map[string]string{
		"title": "x", "start": "25:00", "end": "26:00",
	})); err == nil {
		t.Error("非法时间应拒绝")
	}
	if err := p.EditState(editReq(stateOpEdit, "a404", map[string]string{
		"title": "x", "start": "10:00", "end": "11:00",
	})); err == nil {
		t.Error("不存在的项应拒绝")
	}
	req := editReq(stateOpCancel, "a1", nil)
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
}

func TestPlanEditRequiresTodaysPlan(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	if err := p.EditState(editReq(stateOpCancel, "a1", nil)); err == nil {
		t.Error("今天还没有排表时应拒绝")
	}
}
