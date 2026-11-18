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

// 表单把六项一起带回来：没变的字段不该被刷新更新时间。
func TestUnchangedFieldsKeepUpdatedTime(t *testing.T) {
	p := newTestPlugin(t)
	old := time.Now().Add(-48 * time.Hour)
	if _, _, err := p.storeFor("").Apply(map[string]string{
		"location": "书房", "attire": "灰色毛衣",
	}, old); err != nil {
		t.Fatal(err)
	}
	before, _, err := p.storeFor("").Load()
	if err != nil {
		t.Fatal(err)
	}

	if err := p.EditState(editReq(map[string]string{
		"location": "书房", "attire": "灰色毛衣", "activity": "看电影",
	})); err != nil {
		t.Fatalf("修改 = %v", err)
	}
	after, _, _ := p.storeFor("").Load()
	if !after["location"].Updated.Equal(before["location"].Updated) {
		t.Errorf("没变的字段被刷新了：%v → %v", before["location"].Updated, after["location"].Updated)
	}
	if after["activity"].Text != "看电影" {
		t.Errorf("变化的字段没写进去：%+v", after["activity"])
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
