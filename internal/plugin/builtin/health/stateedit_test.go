package health

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

func healthEditReq(op, row string, values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Row: row, Op: op, Values: values}
}

func seedCondition(t *testing.T, p *Plugin, now time.Time) string {
	t.Helper()
	store := p.storeFor("")
	if _, err := store.Add("感冒", sevModerate.ZH(), now, 3, careTough, now, p.snapshot().limits()); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Current(now)
	if err != nil || len(snap.Active) != 1 {
		t.Fatalf("Current = %+v / %v", snap, err)
	}
	return snap.Active[0].Cond.ID
}

func TestEditConditionFromState(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Now()
	id := seedCondition(t, p, now)

	if err := p.EditState(healthEditReq(stateOpEdit, id, map[string]string{
		"severity": sevMild.ZH(), "care": careMeds,
	})); err != nil {
		t.Fatalf("调整 = %v", err)
	}
	snap, err := p.storeFor("").Current(now)
	if err != nil || len(snap.Active) != 1 {
		t.Fatalf("Current = %+v / %v", snap, err)
	}
	if snap.Active[0].Cond.Care != careMeds {
		t.Errorf("处理方式 = %q", snap.Active[0].Cond.Care)
	}
	if snap.Active[0].Cond.Peak != 30 {
		t.Errorf("严重度峰值 = %d", snap.Active[0].Cond.Peak)
	}
}

// 峰值被「严重度上限」截在规范值之间时，只改处理方式不能把它改回按档位反推的值。
func TestCareOnlyEditKeepsCappedPeak(t *testing.T) {
	p := newTestPlugin(t, nil) // 默认封顶在「难受」档
	now := time.Now()
	store := p.storeFor("")
	if _, err := store.Add("重感冒", sevSevere.ZH(), now, 3, careTough, now, p.snapshot().limits()); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Current(now)
	if err != nil || len(snap.Active) != 1 {
		t.Fatalf("Current = %+v / %v", snap, err)
	}
	cond := snap.Active[0].Cond
	if cond.Peak >= 70 {
		t.Fatalf("封顶没有生效：peak = %d", cond.Peak)
	}

	if err := p.EditState(healthEditReq(stateOpEdit, cond.ID, map[string]string{
		"severity": severityWord(cond.Peak), "care": careMeds,
	})); err != nil {
		t.Fatalf("只改处理方式 = %v", err)
	}
	snap, err = store.Current(now)
	if err != nil || len(snap.Active) != 1 {
		t.Fatalf("Current = %+v / %v", snap, err)
	}
	if snap.Active[0].Cond.Peak != cond.Peak {
		t.Errorf("峰值被顺手改掉了：%d → %d", cond.Peak, snap.Active[0].Cond.Peak)
	}
	if snap.Active[0].Cond.Care != careMeds {
		t.Errorf("处理方式没有改到：%q", snap.Active[0].Cond.Care)
	}
}

func TestRecoverConditionFromState(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Now()
	id := seedCondition(t, p, now)

	if err := p.EditState(healthEditReq(stateOpRecover, id, nil)); err != nil {
		t.Fatalf("痊愈 = %v", err)
	}
	snap, err := p.storeFor("").Current(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Active) != 0 {
		t.Fatalf("痊愈后仍有状况：%+v", snap.Active)
	}
}

func TestHealthEditRejectsBadInput(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Now()
	id := seedCondition(t, p, now)

	if err := p.EditState(healthEditReq(stateOpEdit, id, map[string]string{
		"severity": "不存在的程度", "care": careMeds,
	})); err == nil {
		t.Error("未知严重度应拒绝")
	}
	if err := p.EditState(healthEditReq(stateOpEdit, id, map[string]string{
		"severity": sevMild.ZH(), "care": "不存在的处理",
	})); err == nil {
		t.Error("未知处理方式应拒绝")
	}
	if err := p.EditState(healthEditReq(stateOpEdit, "nope", map[string]string{
		"severity": sevMild.ZH(), "care": careMeds,
	})); err == nil {
		t.Error("不存在的状况应拒绝")
	}
	req := healthEditReq(stateOpRecover, id, nil)
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
}
