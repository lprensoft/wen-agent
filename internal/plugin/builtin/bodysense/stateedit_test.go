package bodysense

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

func editReq(op string, values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Row: "", Op: op, Values: values}
}

func TestSetBodyFromState(t *testing.T) {
	p := newTestPlugin(t, nil)
	if err := p.EditState(editReq(stateOpSet, map[string]string{
		"arousal": "50", "fatigue": "10", "reason": "刚吵完架",
	})); err != nil {
		t.Fatalf("设值 = %v", err)
	}
	now := p.now()
	st, ok, err := p.stateStoreFor("").Current(30, 10, now)
	if err != nil || !ok {
		t.Fatalf("Current = %+v / %v / %v", st, ok, err)
	}
	if st.Arousal != 50 || st.Fatigue != 10 || st.Reason != "刚吵完架" {
		t.Fatalf("落盘 = %+v", st)
	}

	// 两项都设成 0 等于清掉记录，不留下空记录。
	if err := p.EditState(editReq(stateOpSet, map[string]string{"arousal": "0", "fatigue": "0"})); err != nil {
		t.Fatalf("清零 = %v", err)
	}
	if _, ok, _ := p.stateStoreFor("").Current(30, 10, now); ok {
		t.Error("两项都为 0 后不该还有记录")
	}
}

// 两个窗口分别改唤起与疲劳：各自只提交自己动过的那项，合并后两项都在。
func TestBodyFieldsMergeAcrossRequests(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := p.now()
	if _, err := p.stateStoreFor("").Set(30, 20, "跑完步", now); err != nil {
		t.Fatal(err)
	}

	if err := p.EditState(plugin.StateEditRequest{
		Lang: "zh", Section: stateSection, Op: stateOpSet,
		Values:    map[string]string{"arousal": "50", "fatigue": "20", "reason": "跑完步"},
		Originals: map[string]string{"arousal": "30", "fatigue": "20", "reason": "跑完步"},
	}); err != nil {
		t.Fatalf("改唤起 = %v", err)
	}
	if err := p.EditState(plugin.StateEditRequest{
		Lang: "zh", Section: stateSection, Op: stateOpSet,
		Values:    map[string]string{"arousal": "30", "fatigue": "5", "reason": "跑完步"},
		Originals: map[string]string{"arousal": "30", "fatigue": "20", "reason": "跑完步"},
	}); err != nil {
		t.Fatalf("改疲劳 = %v", err)
	}
	st, ok, err := p.stateStoreFor("").Current(30, 10, now)
	if err != nil || !ok {
		t.Fatalf("Current = %+v / %v", st, err)
	}
	if st.Arousal != 50 || st.Fatigue != 5 {
		t.Fatalf("两项没有合并保留：%+v", st)
	}
}

func TestResetBodyFromState(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := p.now()
	if _, _, _, err := p.stateStoreFor("").Apply(30, 20, 30, 30, 10, "跑完步", now); err != nil {
		t.Fatal(err)
	}
	if err := p.EditState(editReq(stateOpReset, nil)); err != nil {
		t.Fatalf("重置 = %v", err)
	}
	if _, ok, _ := p.stateStoreFor("").Current(30, 10, now); ok {
		t.Error("重置后不该还有记录")
	}
}

func TestBodyEditRejectsBadInput(t *testing.T) {
	p := newTestPlugin(t, nil)
	if err := p.EditState(editReq(stateOpSet, map[string]string{"arousal": "101", "fatigue": "0"})); err == nil {
		t.Error("超出范围应拒绝")
	}
	if err := p.EditState(editReq(stateOpSet, map[string]string{"arousal": "", "fatigue": "0"})); err == nil {
		t.Error("空值应拒绝")
	}
	req := editReq(stateOpSet, map[string]string{"arousal": "1", "fatigue": "0"})
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
}

// Set 直接设值并夹到量程内。
func TestStateStoreSetClamps(t *testing.T) {
	dir := t.TempDir()
	s := NewStateStore("zh", dir)
	now := time.Now()
	st, err := s.Set(150, -5, "x", now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Arousal != 100 || st.Fatigue != 0 {
		t.Fatalf("夹取后 = %+v", st)
	}
	got, ok, err := s.Current(30, 10, now)
	if err != nil || !ok || got.Arousal != 100 {
		t.Fatalf("Current = %+v / %v / %v", got, ok, err)
	}
}
