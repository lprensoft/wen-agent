package memory

import (
	"testing"
	"time"

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

	if err := p.EditState(editReq(stateOpEdit, stateSectionIndex, entryID(before),
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

// 标题里带「分类/」前缀时，状态页的操作必须按落盘标识精确命中，不能被
// findEntry 剥前缀后指向另一条同名尾巴的记忆。
func TestIndexEditTargetsExactSlug(t *testing.T) {
	p := stateTestPlugin(t)
	store := p.storeFor("")
	if _, err := store.Save(Entry{Name: "事实/搬家", Description: "旧摘要", Type: "事实"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(Entry{Name: "搬家", Description: "另一条", Type: "事实"}, false); err != nil {
		t.Fatal(err)
	}
	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || len(secs[0].Rows) != 2 {
		t.Fatalf("State = %+v", secs)
	}
	ids := map[string]string{}
	for _, r := range secs[0].Rows {
		ids[r.Value] = r.ID
	}
	if ids["事实/搬家"] == "" || ids["搬家"] == "" || ids["事实/搬家"] == ids["搬家"] {
		t.Fatalf("行身份不对：%+v", ids)
	}

	if err := p.EditState(editReq(stateOpEdit, stateSectionIndex, ids["事实/搬家"],
		map[string]string{"description": "新摘要", "type": "事实"})); err != nil {
		t.Fatalf("改索引 = %v", err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if byName["事实/搬家"].Description != "新摘要" {
		t.Errorf("目标条目没改到：%+v", byName["事实/搬家"])
	}
	if byName["搬家"].Description != "另一条" {
		t.Errorf("另一条被顺手改了：%+v", byName["搬家"])
	}

	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, ids["事实/搬家"], nil)); err != nil {
		t.Fatalf("删除 = %v", err)
	}
	entries, _ = store.List()
	if len(entries) != 1 || entries[0].Name != "搬家" {
		t.Fatalf("删除命中了错条目：%+v", entries)
	}
}

// 只改分类时，摘要这一项原样带回不能覆盖后台刚写的新摘要。
func TestUnchangedIndexFieldNotOverwritten(t *testing.T) {
	p := stateTestPlugin(t)
	store := p.storeFor("")
	if _, err := store.Save(Entry{Name: "喜欢乌龙茶", Description: "旧摘要", Type: "偏好"}, false); err != nil {
		t.Fatal(err)
	}
	// 后台（模型那一侧）在此期间改了摘要。
	if _, err := store.UpdateIndex("喜欢乌龙茶", "后台写的摘要", "偏好", time.Now()); err != nil {
		t.Fatal(err)
	}
	cur, _ := store.Get("喜欢乌龙茶")

	if err := p.EditState(plugin.StateEditRequest{
		Lang: "zh", Domain: "", Section: stateSectionIndex, Row: entryID(cur), Op: stateOpEdit,
		Values:    map[string]string{"description": "旧摘要", "type": "事实"},
		Originals: map[string]string{"description": "旧摘要", "type": "偏好"},
	}); err != nil {
		t.Fatalf("只改分类 = %v", err)
	}
	got, _ := store.Get("喜欢乌龙茶")
	if got.Description != "后台写的摘要" {
		t.Errorf("旧表单覆盖了后台写入的摘要：%q", got.Description)
	}
	if got.Type != "事实" {
		t.Errorf("分类没改到：%q", got.Type)
	}
}

// 旧页面拿的是条目身份：删除后重建的同名/同 slug 条目不能被它删到。
func TestEntryIdentityIsExact(t *testing.T) {
	p := stateTestPlugin(t)
	store := p.storeFor("")
	if _, err := store.Save(Entry{Name: "Trip", Type: "事实"}, false); err != nil {
		t.Fatal(err)
	}
	entries, _ := store.List()
	oldSlug := entryID(entries[0])

	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, oldSlug, nil)); err != nil {
		t.Fatalf("删旧条目 = %v", err)
	}
	if _, err := store.Save(Entry{Name: "trip", Type: "事实"}, false); err != nil {
		t.Fatal(err)
	}
	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, oldSlug, nil)); err == nil {
		t.Error("旧页面凭据不该删到大小写不同的新条目")
	}
	entries, _ = store.List()
	if len(entries) != 1 || entries[0].Name != "trip" {
		t.Fatalf("新条目被误删：%+v", entries)
	}
}

// 两个不同标题折成同一个文件名时，旧页面也只能操作它打开时那一条。
func TestEntryIdentitySurvivesSlugReuse(t *testing.T) {
	p := stateTestPlugin(t)
	store := p.storeFor("")
	if _, err := store.Save(Entry{Name: "周末：搬家", Type: "经历"}, false); err != nil {
		t.Fatal(err)
	}
	entries, _ := store.List()
	oldID := entryID(entries[0])
	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, oldID, nil)); err != nil {
		t.Fatalf("删旧条目 = %v", err)
	}
	if _, err := store.Save(Entry{Name: "周末 搬家", Type: "经历"}, false); err != nil {
		t.Fatal(err)
	}
	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, oldID, nil)); err == nil {
		t.Error("旧页面凭据不该删到复用了同一 slug 的新条目")
	}
	entries, _ = store.List()
	if len(entries) != 1 || entries[0].Name != "周末 搬家" {
		t.Fatalf("新条目被误删：%+v", entries)
	}
}

// 两个请求分别只改摘要和分类：后一个请求不能拿旧快照把前一个的改动盖回去。
func TestIndexFieldsMergeAcrossRequests(t *testing.T) {
	p := stateTestPlugin(t)
	store := p.storeFor("")
	if _, err := store.Save(Entry{Name: "喜欢乌龙茶", Description: "旧摘要", Type: "偏好"}, false); err != nil {
		t.Fatal(err)
	}
	cur, _ := store.Get("喜欢乌龙茶")

	// 第一个窗口：只改摘要。
	if err := p.EditState(plugin.StateEditRequest{
		Lang: "zh", Domain: "", Section: stateSectionIndex, Row: entryID(cur), Op: stateOpEdit,
		Values:    map[string]string{"description": "新摘要", "type": "偏好"},
		Originals: map[string]string{"description": "旧摘要", "type": "偏好"},
	}); err != nil {
		t.Fatalf("改摘要 = %v", err)
	}
	// 第二个窗口（旧快照）：只改分类。
	if err := p.EditState(plugin.StateEditRequest{
		Lang: "zh", Domain: "", Section: stateSectionIndex, Row: entryID(cur), Op: stateOpEdit,
		Values:    map[string]string{"description": "旧摘要", "type": "事实"},
		Originals: map[string]string{"description": "旧摘要", "type": "偏好"},
	}); err != nil {
		t.Fatalf("改分类 = %v", err)
	}
	got, _ := store.Get("喜欢乌龙茶")
	if got.Description != "新摘要" || got.Type != "事实" {
		t.Fatalf("两项没有合并保留：%+v", got)
	}
}

// 日记原样保存不能覆盖另一窗口刚写的新摘要；目标日期不存在仍要报出来。
func TestDiaryUnchangedSummaryNotOverwritten(t *testing.T) {
	p := stateTestPlugin(t)
	dir := plugin.DomainDir(p.libBase, "")
	p.appendTimeline(dir, timelineEntry{Date: "2026-11-17", Summary: "旧摘要"})
	if err := p.updateTimeline(dir, "2026-11-17", "新摘要"); err != nil {
		t.Fatal(err)
	}

	if err := p.EditState(plugin.StateEditRequest{
		Lang: "zh", Domain: "", Section: stateSectionDiary, Row: "2026-11-17", Op: stateOpEdit,
		Values:    map[string]string{"summary": "旧摘要"},
		Originals: map[string]string{"summary": "旧摘要"},
	}); err != nil {
		t.Fatalf("原样保存 = %v", err)
	}
	if got := loadTimeline(dir).Entries; len(got) != 1 || got[0].Summary != "新摘要" {
		t.Fatalf("旧表单覆盖了新摘要：%+v", got)
	}

	// 未修改但目标日期已被删掉：仍然提示刷新，而不是静默成功。
	p.deleteTimeline(dir, "2026-11-17")
	if err := p.EditState(plugin.StateEditRequest{
		Lang: "zh", Domain: "", Section: stateSectionDiary, Row: "2026-11-17", Op: stateOpEdit,
		Values:    map[string]string{"summary": "旧摘要"},
		Originals: map[string]string{"summary": "旧摘要"},
	}); err == nil {
		t.Error("目标日期不存在时应报错")
	}
}

func TestDeleteIndexAndNotReadyTargets(t *testing.T) {
	p := stateTestPlugin(t)
	if _, err := p.storeFor("").Save(Entry{Name: "周末的湖边", Type: "经历"}, false); err != nil {
		t.Fatal(err)
	}
	lake, err := p.storeFor("").Get("周末的湖边")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, entryID(lake), nil)); err != nil {
		t.Fatalf("删除 = %v", err)
	}
	if _, err := p.storeFor("").Get("周末的湖边"); err == nil {
		t.Fatal("删除后仍能读到")
	}
	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, "不存在", nil)); err == nil {
		t.Error("删不存在的记忆应拒绝")
	}
	if err := p.EditState(editReq(stateOpDelete, stateSectionIndex, entryID(lake), nil)); err == nil {
		t.Error("删第二次应拒绝")
	}
	req := editReq(stateOpEdit, stateSectionIndex, entryID(lake), map[string]string{"description": "x"})
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
