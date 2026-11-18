package bookshelf

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

func editReq(op, row string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Row: row, Op: op}
}

func TestResetProgressFromState(t *testing.T) {
	p, _ := newPlugin(t, nil)
	store := p.storeFor("")
	now := time.Now()
	if err := store.Update(func(st *State) error {
		st.Current = "边城.txt"
		st.Books["边城.txt"] = &Progress{
			Offset: 50, Length: 100, Started: now, LastRead: now,
			Notes: []Note{{Text: "翠翠", At: now, Percent: 50}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := p.EditState(editReq(stateOpReset, "边城.txt")); err != nil {
		t.Fatalf("重置 = %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Books["边城.txt"]; ok {
		t.Fatalf("重置后仍有进度：%+v", st.Books)
	}
	if st.Current != "" {
		t.Errorf("Current 应一并清掉：%q", st.Current)
	}
}

func TestResetProgressRejectsBadTarget(t *testing.T) {
	p, _ := newPlugin(t, nil)
	if err := p.EditState(editReq(stateOpReset, "不存在.txt")); err == nil {
		t.Error("没有进度的书应拒绝")
	}
	req := editReq(stateOpReset, "边城.txt")
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
	if err := p.EditState(plugin.StateEditRequest{Lang: "zh", Section: "shelf", Op: "nope"}); err == nil {
		t.Error("未知操作应拒绝")
	}
}
