package growth

import (
	"testing"

	"wen/internal/plugin"
)

func editReq(op, row string, values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Row: row, Op: op, Values: values}
}

func seedDetail(t *testing.T, p *Plugin) {
	t.Helper()
	if _, err := p.storeFor("").Update(func(st *State) (bool, []LogEntry, error) {
		st.Details = []Entry{{ID: "d1", Text: "开始喝无糖可乐", Basis: "上周说想少糖", Why: "还是那个爱尝新的人"}}
		st.Quota = Quota{Date: "2026-10-20", Used: 2}
		return true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 人工改枝叶：落盘、留记录、进可撤销历史，但不占每日限额也不要求反思轮次。
func TestEditDetailFromState(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	seedDetail(t, p)

	if err := p.EditState(editReq(stateOpEdit, "d1", map[string]string{
		"text": "开始喝无糖气泡水", "basis": "上周说想少糖", "why": "还是那个爱尝新的人",
	})); err != nil {
		t.Fatalf("EditState = %v", err)
	}
	st, err := p.storeFor("").Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Details) != 1 || st.Details[0].Text != "开始喝无糖气泡水" {
		t.Fatalf("枝叶 = %+v", st.Details)
	}
	if len(st.History) != 1 || st.History[0].Op != opModify {
		t.Fatalf("历史 = %+v", st.History)
	}
	if st.Quota.Used != 2 {
		t.Errorf("人工修正不该占每日限额：%d", st.Quota.Used)
	}
}

func TestLetGoDetailFromState(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	seedDetail(t, p)
	if err := p.EditState(editReq(stateOpLetGo, "d1", nil)); err != nil {
		t.Fatalf("放下 = %v", err)
	}
	st, _ := p.storeFor("").Load()
	if len(st.Details) != 0 {
		t.Fatalf("放下后枝叶 = %+v", st.Details)
	}
	if len(st.History) != 1 || st.History[0].Op != opLetGo {
		t.Fatalf("历史 = %+v", st.History)
	}
	if err := p.EditState(editReq(stateOpLetGo, "d1", nil)); err == nil {
		t.Error("放下已经不在的条目应拒绝")
	}
}

// 批准/驳回由行身份指回那一条提议，与设置页按钮同一条规则。
func TestPendingDecisionFromState(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	pending := Entry{ID: "c3", Nonce: "abc123", Text: "说话不再绕弯", Basis: "反复被提醒", Why: "骨子里其实直"}
	if _, err := p.storeFor("").Update(func(st *State) (bool, []LogEntry, error) {
		e := pending
		st.Pending = &e
		return true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := p.EditState(editReq(stateOpApprove, "abc123", nil)); err != nil {
		t.Fatalf("批准 = %v", err)
	}
	st, _ := p.storeFor("").Load()
	if st.Pending != nil || len(st.Core) != 1 || st.Core[0].Text != pending.Text {
		t.Fatalf("批准后 = %+v", st)
	}

	// 同一条凭据再用一次，已经不在等了。
	if err := p.EditState(editReq(stateOpApprove, "abc123", nil)); err == nil {
		t.Error("已经处理过的提议再批应拒绝")
	}
}

func TestGrowthEditRejectsUnknownTarget(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	req := editReq(stateOpEdit, "d1", map[string]string{"text": "x", "basis": "y", "why": "z"})
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
	if err := p.EditState(editReq(stateOpEdit, "d404", map[string]string{"text": "x", "basis": "y", "why": "z"})); err == nil {
		t.Error("不存在的小变化应拒绝")
	}
	if err := p.EditState(editReq(stateOpEdit, "d1", map[string]string{"text": " ", "basis": "y", "why": "z"})); err == nil {
		t.Error("空内容应拒绝")
	}
}
