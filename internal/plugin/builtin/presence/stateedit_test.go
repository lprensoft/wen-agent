package presence

import (
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func editReq(values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Op: stateOpEdit, Values: values}
}

func TestEditPresenceFromState(t *testing.T) {
	p := newTestPlugin(t)
	if _, _, err := p.storeFor("").Apply(map[string]string{
		"location": "书房", "attire": "灰色毛衣", "activity": "写东西",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := p.EditState(editReq(map[string]string{
		"location": "客厅", "attire": "", "activity": "看电影",
	})); err != nil {
		t.Fatalf("修改 = %v", err)
	}
	snap, _, err := p.storeFor("").Load()
	if err != nil {
		t.Fatal(err)
	}
	if snap["location"].Text != "客厅" || snap["activity"].Text != "看电影" {
		t.Fatalf("修改后 = %+v", snap)
	}
	if _, ok := snap["attire"]; ok {
		t.Errorf("空值应清除该字段：%+v", snap)
	}
}

func TestPresenceEditRejectsBadInput(t *testing.T) {
	p := newTestPlugin(t)
	if err := p.EditState(editReq(map[string]string{"focus": strings.Repeat("字", 81)})); err == nil {
		t.Error("超长字段应拒绝")
	}
	req := editReq(map[string]string{"location": "客厅"})
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
	req = editReq(map[string]string{"location": "客厅"})
	req.Op = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("未知操作应拒绝")
	}
}
