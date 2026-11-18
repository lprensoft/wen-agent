package agenda

import (
	"testing"
	"time"

	"wen/internal/availability"
	"wen/internal/cue"
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

// itemToken 取当前表里某一项的操作凭据（表版本 + 修订号 + 序号）。
func itemToken(t *testing.T, p *Plugin, id string) string {
	t.Helper()
	pl, err := p.storeFor("").LoadPlan()
	if err != nil {
		t.Fatal(err)
	}
	return planToken(pl, id)
}

func TestEditPlanItemFromState(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	seedPlan(t, p, now)

	if err := p.EditState(editReq(stateOpEdit, itemToken(t, p, "a1"), map[string]string{
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

	if err := p.EditState(editReq(stateOpCancel, itemToken(t, p, "a1"), nil)); err != nil {
		t.Fatalf("取消 = %v", err)
	}
	pl, _ := p.storeFor("").LoadPlan()
	if it := pl.item("a1"); it == nil || it.Status != statusCancelled || it.Outcome == "" {
		t.Fatalf("取消后 = %+v", it)
	}
	// 已经结束的项不能再取消。
	if err := p.EditState(editReq(stateOpCancel, itemToken(t, p, "a2"), nil)); err == nil {
		t.Error("已做完的项应拒绝")
	}
}

// 取消一项正在进行中的活动，要把它占着的忙碌公告板一并撤掉。
func TestCancelOngoingClearsAvailability(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	seedPlan(t, p, now)
	if _, err := p.storeFor("").UpdatePlan(func(pl *Plan) (bool, error) {
		it := pl.item("a1")
		it.Status = statusOngoing
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	availability.Clear(availabilitySource)
	defer availability.Clear(availabilitySource)
	availability.Set(availability.State{
		Source: availabilitySource, Activity: "整理提案", Level: availability.Heavy,
		Since: now, Until: now.Add(time.Hour),
	})

	if err := p.EditState(editReq(stateOpCancel, itemToken(t, p, "a1"), nil)); err != nil {
		t.Fatalf("取消 = %v", err)
	}
	if _, ok := availability.Current(now); ok {
		t.Error("取消后仍占着忙碌公告板")
	}
}

// 改期要撤回旧时间已经投出的「快到了」，不能等心跳把旧提醒领走。
func TestRescheduleDropsOldSoonCue(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	seedPlan(t, p, now)
	cue.Reset()
	defer cue.Reset()
	cue.Post(cue.Cue{
		Source: availabilitySource, Key: "soon|a1", Text: "快到了",
		Expire: now.Add(time.Hour),
	})

	if err := p.EditState(editReq(stateOpEdit, itemToken(t, p, "a1"), map[string]string{
		"title": "整理提案", "start": "14:00", "end": "15:00", "place": "书房",
	})); err != nil {
		t.Fatalf("改期 = %v", err)
	}
	if got := cue.Take(now); len(got) != 0 {
		t.Fatalf("改期后仍有旧提醒：%+v", got)
	}
}

func TestPlanEditRejectsBadInput(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	seedPlan(t, p, now)

	if err := p.EditState(editReq(stateOpEdit, itemToken(t, p, "a1"), map[string]string{
		"title": "x", "start": "25:00", "end": "26:00",
	})); err == nil {
		t.Error("非法时间应拒绝")
	}
	if err := p.EditState(editReq(stateOpEdit, planToken(Plan{Date: p.snapshot().today(now).Format(dateLayout), PlannedAt: now}, "a404"), map[string]string{
		"title": "x", "start": "10:00", "end": "11:00",
	})); err == nil {
		t.Error("不存在的项应拒绝")
	}
	req := editReq(stateOpCancel, itemToken(t, p, "a1"), nil)
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
}

func TestPlanEditRequiresTodaysPlan(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	if err := p.EditState(editReq(stateOpCancel, "0|0|a1", nil)); err == nil {
		t.Error("今天还没有排表时应拒绝")
	}
}

// 旧页面上的按钮不能落到重排或次日排表后占用同一序号的那一项上。
func TestStalePlanTokenRejected(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	seedPlan(t, p, now)

	// 同一份表被后台改过（例如调度器标了另一项已开始）：修订号变化。
	old := itemToken(t, p, "a1")
	if _, err := p.storeFor("").UpdatePlan(func(pl *Plan) (bool, error) {
		pl.item("a2").Title = "做晚饭（改）"
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.EditState(editReq(stateOpCancel, old, nil)); err == nil {
		t.Error("表改过之后旧凭据应被拒绝")
	}
	pl, _ := p.storeFor("").LoadPlan()
	if it := pl.item("a1"); it == nil || it.Status != statusPlanned {
		t.Fatalf("旧凭据仍然取消到了活动：%+v", it)
	}

	// 同日重排：序号还是 a1，但那是另一张表。
	old = itemToken(t, p, "a1")
	day := p.snapshot().today(now)
	if err := p.storeFor("").SavePlan(Plan{
		Date: day.Format(dateLayout), PlannedAt: now.Add(time.Minute),
		Items: []Item{{ID: "a1", Title: "新的一天", Start: "09:00", End: "10:00", Status: statusPlanned}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.EditState(editReq(stateOpCancel, old, nil)); err == nil {
		t.Error("重排之后旧凭据应被拒绝")
	}
	pl, _ = p.storeFor("").LoadPlan()
	if it := pl.item("a1"); it == nil || it.Status != statusPlanned || it.Title != "新的一天" {
		t.Fatalf("旧凭据取消到了重排后的项：%+v", it)
	}
}
