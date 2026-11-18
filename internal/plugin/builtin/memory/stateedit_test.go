package memory

import (
	"testing"

	"wen/internal/plugin"
)

func editReq(op, section, row string, values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: section, Row: row, Op: op, Values: values}
}

// 改索引只动摘要与分类：正文、淡忘标记与「最后一次用到」都保持原样。
func TestEditIndexKeepsBodyAndDecay(t *testing.T) {
	p := stateTestPlugin(t)
	if _, err := p.storeFor("").Save(Entry{
		Name: "喜欢乌龙茶", Description: "不加糖", Type: "偏好", Content: "只喝无糖的",
	}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.storeFor("").Blur("喜欢乌龙茶"); err != nil {
		t.Fatal(err)
	}
	before, err := p.storeFor("").Get("喜欢乌龙茶")
	if err != nil {
		t.Fatal(err)
	}

	if err := p.EditState(editReq(stateOpEdit, stateSectionIndex, "喜欢乌龙茶",
		map[string]string{"description": "只喝无糖的茶", "type": "事实"})); err != nil {
		t.Fatalf("EditState = %v", err)
	}
	after, err := p.storeFor("").Get("喜欢乌龙茶")
	if err != nil {
		t.Fatal(err)
	}
	if after.Description != "只喝无糖的茶" || after.Type != "事实" {
		t.Fatalf("索引没有改到：%+v", after)
	}
	if after.Content != before.Content {
		t.Errorf("正文被改动了：%q → %q", before.Content, after.Content)
	}
	if !after.Blurred {
		t.Error("一次索引修正不该把淡忘的记忆复活")
	}
	if !after.LastUsed.Equal(before.LastUsed) {
		t.Errorf("LastUsed 被改动了：%v → %v", before.LastUsed, after.LastUsed)
	}
}

func TestDeleteIndexAndNotReadyTargets(t *testing.T) {
	p := stateTestPlugin(t)
	if _, err := p.storeFor("").Save(Entry{Name: "周末的湖边", Type: "经历"}, false); err != nil {
		t.Fatal(err)
	}
	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, "周末的湖边", nil)); err != nil {
		t.Fatalf("删除 = %v", err)
	}
	if _, err := p.storeFor("").Get("周末的湖边"); err == nil {
		t.Fatal("删除后仍能读到")
	}
	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, "不存在", nil)); err == nil {
		t.Error("删不存在的记忆应拒绝")
	}
	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, "周末的湖边", nil)); err == nil {
		t.Error("删第二次应拒绝")
	}
	req := editReq(stateOpEdit, stateSectionIndex, "周末的湖边", map[string]string{"description": "x"})
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
}

func TestEditDiary(t *testing.T) {
	p := stateTestPlugin(t)
	dir := plugin.DomainDir(p.libBase, "")
	p.appendTimeline(dir, timelineEntry{Date: "2026-11-16", Summary: "陪她去医院复查", Facts: []string{"天气：阴"}})

	if err := p.EditState(editReq(stateOpEdit, stateSectionDiary, "2026-11-16",
		map[string]string{"summary": "陪她去医院复查，结果很好"})); err != nil {
		t.Fatalf("改日记 = %v", err)
	}
	entries := loadTimeline(dir).Entries
	if len(entries) != 1 || entries[0].Summary != "陪她去医院复查，结果很好" {
		t.Fatalf("时间线 = %+v", entries)
	}
	if len(entries[0].Facts) != 1 {
		t.Errorf("当日事实不应被改动：%+v", entries[0])
	}

	if err := p.EditState(editReq(stateOpDelete, stateSectionDiary, "2026-11-16", nil)); err != nil {
		t.Fatalf("删日记 = %v", err)
	}
	if got := loadTimeline(dir).Entries; len(got) != 0 {
		t.Fatalf("删除后时间线 = %+v", got)
	}
	if err := p.EditState(editReq(stateOpDelete, stateSectionDiary, "2026-11-16", nil)); err == nil {
		t.Error("删已经不存在的一天应拒绝")
	}
	if err := p.EditState(editReq(stateOpEdit, stateSectionDiary, "不是日期",
		map[string]string{"summary": "x"})); err == nil {
		t.Error("非法日期应拒绝")
	}
}
