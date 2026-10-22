package bodysense

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func newTestPlugin(t *testing.T, cfg map[string]any) *Plugin {
	t.Helper()
	p := New()
	// 钉在白天、骰子永不出「一阵困意」：困意随钟点变，不钉住的话夜里跑测试会多出一行
	p.now = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local) }
	p.roll = func() float64 { return 1 }
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, cfg); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	return p
}

func scoped(write string, read []string) context.Context {
	return plugin.WithScope(context.Background(), plugin.Scope{Write: write, Read: read})
}

func TestInitRejectsWithoutStateDir(t *testing.T) {
	if err := New().Init(plugin.InitContext{}, nil); err == nil {
		t.Fatal("没有持久化目录时应当拒绝启用")
	}
}

func TestInitUsesDefaultsWhenKeyMissing(t *testing.T) {
	p := newTestPlugin(t, nil)
	if len(p.snapshot().parts) == 0 {
		t.Fatal("配置缺失时应当回落到内置默认部位表")
	}
}

// FieldText 的空串是合法取值：用户清空某一档保存后，那一档就该是空的。
// 若这里用 plugin.CfgString（它把空串当缺失），默认值会悄悄长回来。
func TestInitEmptyGroupIsNotRestoredToDefault(t *testing.T) {
	p := newTestPlugin(t, map[string]any{
		"parts_daily":    "手",
		"parts_close":    "",
		"parts_intimate": "",
		"parts_private":  "",
	})
	parts := p.snapshot().parts
	if len(parts) != 1 || parts[0].Name != "手" {
		t.Fatalf("部位表 = %+v，期望只剩用户填的那一条", parts)
	}
}

func TestInitRejectsBadParts(t *testing.T) {
	err := New().Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"parts_daily": "", "parts_close": "", "parts_intimate": "", "parts_private": "",
	})
	if err == nil {
		t.Fatal("四档全空时应当报错，让设置页把错误显示出来")
	}
}

// Init 会在运行时被 SetConfig 反复调用，改配置后旧的域库表必须被换掉。
func TestInitIsReentrant(t *testing.T) {
	dir := t.TempDir()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: dir}, nil); err != nil {
		t.Fatal(err)
	}
	old := p.storeFor("")
	if err := p.Init(plugin.InitContext{StateDir: dir}, map[string]any{
		"parts_daily": "手", "parts_close": "", "parts_intimate": "", "parts_private": "",
		"familiarity_pace": paceFast,
	}); err != nil {
		t.Fatal(err)
	}
	if p.storeFor("") == old {
		t.Error("重新 Init 后域库表应当被重建")
	}
	if p.snapshot().pace != paceFast {
		t.Error("新配置未生效")
	}
}

func TestSystemPromptIsStaticAndCheap(t *testing.T) {
	// 未 Init 的插件也会被列表接口调到，不能崩也不能读盘
	got := New().SystemPrompt()
	for _, want := range []string{"record_touch", "先用 record_touch", "初次", "习以为常"} {
		if !strings.Contains(got, want) {
			t.Errorf("system 提示词缺少 %q", want)
		}
	}
}

func TestTurnPromptEmptyWhenNoRecords(t *testing.T) {
	p := newTestPlugin(t, nil)
	got, err := p.TurnPrompt(scoped("", nil), plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("没有记录时不该注入，得到 %q", got)
	}
}

func TestTurnPromptRendersRecords(t *testing.T) {
	p := newTestPlugin(t, nil)
	ctx := scoped("", nil)
	record(t, p, ctx, Touch{Part: "手", Action: "牵"})
	record(t, p, ctx, Touch{Part: "腰", Action: "揽"})

	got, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[身体接触记录]", "手：1 次（初次）· 日常", "腰：1 次（初次）· 亲密", "最近一次接触：腰 · 揽 · 刚刚"} {
		if !strings.Contains(got, want) {
			t.Errorf("注入内容缺少 %q，实际:\n%s", want, got)
		}
	}
}

// ---------- 跨可见域 ----------

func TestVisibleViewsSumsAcrossReadableDomains(t *testing.T) {
	p := newTestPlugin(t, nil)
	shared := scoped("", []string{})
	inner := scoped("inner", []string{"inner"})

	for i := 0; i < 5; i++ {
		record(t, p, shared, Touch{Part: "手", Action: "牵"})
	}
	for i := 0; i < 3; i++ {
		record(t, p, inner, Touch{Part: "手", Action: "扣紧"})
	}

	// 里人格看到的是同一具身体：共享 5 + 自己 3
	if v := viewOf(t, p, inner, "手"); v.Count != 8 {
		t.Errorf("里人格看到 %d 次，期望 8 次（求和）", v.Count)
	}
	// 表人格只看到共享域的那 5 次
	if v := viewOf(t, p, shared, "手"); v.Count != 5 {
		t.Errorf("表人格看到 %d 次，期望 5 次", v.Count)
	}
}

// LastAct 是要进注入文本的自由文本，只能取写入域那一条，
// 否则不可读域的动作描述会随时间戳漏出来。
func TestLastActComesFromWriteDomainOnly(t *testing.T) {
	p := newTestPlugin(t, nil)
	shared := scoped("", []string{})
	inner := scoped("inner", []string{"inner"})

	record(t, p, shared, Touch{Part: "手", Action: "牵"})
	record(t, p, inner, Touch{Part: "手", Action: "只有里人格知道"})

	if v := viewOf(t, p, shared, "手"); v.LastAct != "牵" {
		t.Errorf("表人格看到的方式 = %q，期望「牵」", v.LastAct)
	}
	got, err := p.TurnPrompt(shared, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "只有里人格知道") {
		t.Errorf("不可读域的方式泄漏进了注入文本:\n%s", got)
	}
}

// 不可读域里独有的部位不该以任何形式出现——注入、list、条数都不行。
func TestUnreadableDomainLeaksNothing(t *testing.T) {
	p := newTestPlugin(t, nil)
	shared := scoped("", []string{})
	inner := scoped("inner", []string{"inner"})

	record(t, p, shared, Touch{Part: "手"})
	record(t, p, inner, Touch{Part: "腰"})

	prompt, err := p.TurnPrompt(shared, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	list, err := (&listTool{p: p}).Execute(shared, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{prompt, list} {
		if strings.Contains(out, "腰") {
			t.Errorf("不可读域的部位名泄漏:\n%s", out)
		}
	}
	if !strings.Contains(list, "共 1 个部位") {
		t.Errorf("条数应当只算可读域，实际:\n%s", list)
	}
}

// ---------- 注入渲染的降级 ----------

func TestRenderViewsDegrades(t *testing.T) {
	views := []view{
		{Name: "手", Privacy: privacyDaily, Count: 20, Stage: stageUsual, Last: time.Now()},
		{Name: "腰", Privacy: privacyIntimate, Count: 2, Stage: stageStrange, Last: time.Now()},
	}
	full := renderViews("", views, 0)
	if !strings.Contains(full, "20 次") || !strings.Contains(full, "最近一次接触") {
		t.Fatalf("不限预算时应当全列:\n%s", full)
	}

	stages := renderViews("", views, len(renderStages("", views)))
	if strings.Contains(stages, "20 次") || !strings.Contains(stages, "习以为常") {
		t.Errorf("第二级应当去掉次数只留阶段:\n%s", stages)
	}

	grouped := renderViews("", views, len(renderGrouped("", views)))
	if !strings.Contains(grouped, "习以为常：手") || !strings.Contains(grouped, "生疏：腰") {
		t.Errorf("第三级应当按阶段归并:\n%s", grouped)
	}

	minimal := renderViews("", views, 10)
	if !strings.Contains(minimal, "共 2 个部位") {
		t.Errorf("第四级应当只报部位数:\n%s", minimal)
	}
}

// 时间戳为零值（旧文件、手改坏的文件）时整行不出现，不编一个出来。
func TestLastTouchLineOmittedWhenTimestampMissing(t *testing.T) {
	views := []view{{Name: "手", Count: 3, Stage: stageStrange}}
	if line := lastTouchLine("", views); line != "" {
		t.Errorf("零值时间戳应当整行不出现，得到 %q", line)
	}
}

// ---------- 部位表改动留下的孤儿 ----------

func TestOrphanRecordsAreKeptButNotInjected(t *testing.T) {
	dir := t.TempDir()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: dir}, map[string]any{
		"parts_daily": "手\n肩", "parts_close": "", "parts_intimate": "", "parts_private": "",
	}); err != nil {
		t.Fatal(err)
	}
	ctx := scoped("", nil)
	record(t, p, ctx, Touch{Part: "手"})
	record(t, p, ctx, Touch{Part: "肩"})

	// 把「肩」从部位表里去掉
	if err := p.Init(plugin.InitContext{StateDir: dir}, map[string]any{
		"parts_daily": "手", "parts_close": "", "parts_intimate": "", "parts_private": "",
	}); err != nil {
		t.Fatal(err)
	}

	prompt, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "肩") {
		t.Errorf("表外的部位不该进注入:\n%s", prompt)
	}
	list, err := (&listTool{p: p}).Execute(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "另有 1 条记录的部位已不在当前部位表中") {
		t.Errorf("孤儿条目应当在 list 里提一句:\n%s", list)
	}
	// 数据仍在盘上：把名字改回去就该恢复
	if err := p.Init(plugin.InitContext{StateDir: dir}, map[string]any{
		"parts_daily": "手\n肩", "parts_close": "", "parts_intimate": "", "parts_private": "",
	}); err != nil {
		t.Fatal(err)
	}
	if v := viewOf(t, p, ctx, "肩"); v.Count != 1 {
		t.Error("改回部位名后旧计数应当恢复")
	}
}

// ---------- 工具 ----------

func TestRecordToolRejectsUnknownPartAndListsOptions(t *testing.T) {
	p := newTestPlugin(t, map[string]any{
		"parts_daily": "手", "parts_close": "脸颊", "parts_intimate": "", "parts_private": "",
	})
	_, err := (&recordTool{p: p}).Execute(scoped("", nil), json.RawMessage(`{"touches":[{"part":"尾巴"}]}`))
	if err == nil {
		t.Fatal("表外的部位应当报错")
	}
	// 列出的必须是配置表里的部位，而不是「已记录的部位」——后者会泄漏存在性
	if !strings.Contains(err.Error(), "手") || !strings.Contains(err.Error(), "脸颊") {
		t.Errorf("报错应当列出可选部位，实际: %v", err)
	}
}

func TestRecordToolLimitsBatchSize(t *testing.T) {
	p := newTestPlugin(t, nil)
	var items []string
	for i := 0; i < maxTouchesPerCall+1; i++ {
		items = append(items, `{"part":"手"}`)
	}
	args := json.RawMessage(`{"touches":[` + strings.Join(items, ",") + `]}`)
	if _, err := (&recordTool{p: p}).Execute(scoped("", nil), args); err == nil {
		t.Fatal("超过一次上报的条数上限应当报错")
	}
	if _, err := (&recordTool{p: p}).Execute(scoped("", nil), json.RawMessage(`{"touches":[]}`)); err == nil {
		t.Fatal("空上报应当报错")
	}
}

// 回显必须与 [身体接触记录] 的口径一致：求和后的值，不是写入域自己的计数。
func TestRecordToolEchoesSummedCount(t *testing.T) {
	p := newTestPlugin(t, nil)
	shared := scoped("", []string{})
	inner := scoped("inner", []string{"inner"})
	for i := 0; i < 5; i++ {
		record(t, p, shared, Touch{Part: "手"})
	}
	out, err := (&recordTool{p: p}).Execute(inner, json.RawMessage(`{"touches":[{"part":"手","action":"握"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "手 ×6") {
		t.Errorf("回显 = %q，期望是共享域 5 次 + 本次 1 次的求和", out)
	}
}

// 部位表改了，工具声明里的 enum 要跟着变——Agent 每次请求都重新取 Schema。
func TestRecordToolSchemaEnumFollowsConfig(t *testing.T) {
	dir := t.TempDir()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: dir}, map[string]any{
		"parts_daily": "手", "parts_close": "", "parts_intimate": "", "parts_private": "",
	}); err != nil {
		t.Fatal(err)
	}
	tool := &recordTool{p: p}
	if got := string(tool.Schema()); !strings.Contains(got, `"手"`) || strings.Contains(got, `"肩"`) {
		t.Fatalf("初始 enum 不对:\n%s", got)
	}
	if err := p.Init(plugin.InitContext{StateDir: dir}, map[string]any{
		"parts_daily": "肩", "parts_close": "", "parts_intimate": "", "parts_private": "",
	}); err != nil {
		t.Fatal(err)
	}
	if got := string(tool.Schema()); !strings.Contains(got, `"肩"`) || strings.Contains(got, `"手"`) {
		t.Fatalf("改配置后 enum 未跟随:\n%s", got)
	}
	var probe map[string]any
	if err := json.Unmarshal(tool.Schema(), &probe); err != nil {
		t.Fatalf("schema 不是合法 JSON: %v", err)
	}
}

func TestListToolEmpty(t *testing.T) {
	p := newTestPlugin(t, nil)
	got, err := (&listTool{p: p}).Execute(scoped("", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "还没有任何部位的接触记录") {
		t.Errorf("空库输出 = %q", got)
	}
}

// ---------- 设置页操作 ----------

func TestClearActionWipesAllDomains(t *testing.T) {
	p := newTestPlugin(t, nil)
	shared := scoped("", []string{})
	inner := scoped("inner", []string{"inner"})
	record(t, p, shared, Touch{Part: "手"})
	record(t, p, inner, Touch{Part: "腰"})

	if st, _ := p.ActionState(actionClear); st.Status != plugin.ActionIdle {
		t.Errorf("初始状态 = %q，期望 idle", st.Status)
	}
	if err := p.StartAction(context.Background(), actionClear); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if st, _ := p.ActionState(actionClear); st.Status != plugin.ActionDone {
		t.Errorf("清空后状态 = %q，期望 done", st.Status)
	}
	for _, ctx := range []context.Context{shared, inner} {
		views, _, err := p.visibleViews(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 0 {
			t.Errorf("清空后仍有记录: %+v", views)
		}
	}
	if err := p.StartAction(context.Background(), "不存在"); err == nil {
		t.Error("未知操作应当报错")
	}
}

// ---------- 辅助 ----------

func record(t *testing.T, p *Plugin, ctx context.Context, touches ...Touch) {
	t.Helper()
	store := p.writeStore(ctx)
	if store == nil {
		t.Fatal("拿不到写入域的记录库")
	}
	if _, err := store.Record(touches); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
}

func viewOf(t *testing.T, p *Plugin, ctx context.Context, name string) view {
	t.Helper()
	views, _, err := p.visibleViews(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	for _, v := range views {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("没有找到部位 %q，实际有 %+v", name, views)
	return view{}
}
