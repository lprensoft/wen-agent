package mood

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

func moodEditReq(op string, values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Op: op, Values: values}
}

func TestSetMoodFromState(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Now()
	if _, err := p.storeFor("").Set(30, "还行", now); err != nil {
		t.Fatal(err)
	}

	if err := p.EditState(moodEditReq(stateOpSet, map[string]string{
		"value": "60", "reason": "被你夸了",
	})); err != nil {
		t.Fatalf("设值 = %v", err)
	}
	st, ok, err := p.storeFor("").Current(p.snapshot().decayPerHour, time.Now())
	if err != nil || !ok {
		t.Fatalf("Current = %v / %v", ok, err)
	}
	if st.Value != 60 || st.Reason != "被你夸了" {
		t.Fatalf("落盘 = %+v", st)
	}

	if err := p.EditState(moodEditReq(stateOpReset, nil)); err != nil {
		t.Fatalf("重置 = %v", err)
	}
	if _, ok, _ := p.storeFor("").Current(p.snapshot().decayPerHour, time.Now()); ok {
		t.Error("重置后不该还有记录")
	}
}

func TestMoodEditRejectsBadInput(t *testing.T) {
	p := newTestPlugin(t, nil)
	if err := p.EditState(moodEditReq(stateOpSet, map[string]string{"value": "101"})); err == nil {
		t.Error("超出范围应拒绝")
	}
	if err := p.EditState(moodEditReq(stateOpSet, map[string]string{"value": ""})); err == nil {
		t.Error("空值应拒绝")
	}
	req := moodEditReq(stateOpSet, map[string]string{"value": "1"})
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
}
