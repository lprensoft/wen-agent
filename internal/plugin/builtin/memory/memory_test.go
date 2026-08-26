package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/plugin"
)

// newTestPlugin 返回一个已初始化到临时目录的插件。
func newTestPlugin(t *testing.T, cfg map[string]any) *Plugin {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, cfg); err != nil {
		t.Fatal(err)
	}
	return p
}

// indexPrompt 返回本轮注入的记忆索引。
// 索引由 TurnPrompt 注入而不是 SystemPrompt：它的内容取决于本轮的可见域。
func indexPrompt(t *testing.T, p *Plugin) string {
	t.Helper()
	s, err := p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// seed 写入 n 条记忆，标题按序号补零保证排序稳定。
func seed(t *testing.T, p *Plugin, n int) {
	t.Helper()
	s := p.snapshot()
	for i := range n {
		_, err := s.store.Save(Entry{
			Name:        fmt.Sprintf("条目%03d", i),
			Description: fmt.Sprintf("这是第 %d 条记忆的摘要说明文字", i),
			Type:        Types[i%len(Types)],
			Content:     "正文",
		}, false)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestInitRequiresStateDir(t *testing.T) {
	p := New()
	if err := p.Init(plugin.InitContext{Workdir: "/w"}, nil); err == nil {
		t.Fatal("没有持久化目录时应拒绝启用，而不是退化到进程当前目录")
	}
	if indexPrompt(t, p) != "" {
		t.Error("未初始化时不应注入索引，也不应产生磁盘访问")
	}
}

func TestScopeDecidesMemoryDir(t *testing.T) {
	state, work := t.TempDir(), t.TempDir()
	ictx := plugin.InitContext{StateDir: state, Workdir: work}

	global := New()
	if err := global.Init(ictx, map[string]any{"scope": "global"}); err != nil {
		t.Fatal(err)
	}
	if got, want := global.snapshot().store.Dir(), filepath.Join(state, "memories"); got != want {
		t.Errorf("全局库目录 = %q, want %q", got, want)
	}

	proj := New()
	if err := proj.Init(ictx, map[string]any{"scope": "project"}); err != nil {
		t.Fatal(err)
	}
	if got, want := proj.snapshot().store.Dir(), filepath.Join(work, ".wen", "memories"); got != want {
		t.Errorf("项目库目录 = %q, want %q", got, want)
	}

	// 两个范围互不可见
	global.snapshot().store.Save(Entry{Name: "全局条目", Type: "事实", Content: "c"}, false)
	proj.snapshot().store.Save(Entry{Name: "项目条目", Type: "事实", Content: "c"}, false)
	if s := indexPrompt(t, global); !strings.Contains(s, "全局条目") || strings.Contains(s, "项目条目") {
		t.Errorf("全局库不应看到项目记忆:\n%s", s)
	}
	if s := indexPrompt(t, proj); !strings.Contains(s, "项目条目") || strings.Contains(s, "全局条目") {
		t.Errorf("项目库不应看到全局记忆:\n%s", s)
	}
}

func TestScopeProjectRequiresWorkdir(t *testing.T) {
	p := New()
	err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"scope": "project"})
	if err == nil {
		t.Fatal("按项目分库但没有工作目录时应拒绝启用")
	}
	if !strings.Contains(err.Error(), "工作目录") {
		t.Errorf("错误信息应说明原因: %v", err)
	}

	// 反过来：没有 StateDir 时项目范围仍可用（记忆落在工作目录下）
	q := New()
	if err := q.Init(plugin.InitContext{Workdir: t.TempDir()}, map[string]any{"scope": "project"}); err != nil {
		t.Errorf("项目范围不应依赖 StateDir: %v", err)
	}
}

func TestSystemPromptGuideOnlyWithoutMemories(t *testing.T) {
	p := newTestPlugin(t, nil)
	// 判据是引导保存第一条记忆的东西，空库时最需要它，因此它是静态注入的
	if got := p.SystemPrompt(); !strings.Contains(got, "save_memory") {
		t.Errorf("空记忆库仍应注入保存判据:\n%s", got)
	}
	// 但不应出现空的索引标题
	if got := indexPrompt(t, p); got != "" {
		t.Errorf("空记忆库不应注入索引部分:\n%s", got)
	}
}

func TestSystemPromptStaysDiskFree(t *testing.T) {
	// 列表接口会对禁用的插件也调用 SystemPrompt()，它必须廉价且无副作用。
	// 索引改道 TurnPrompt 之后这一点是白拿的，用测试把它固定住。
	p := newTestPlugin(t, nil)
	seed(t, p, 3)
	if got := p.SystemPrompt(); strings.Contains(got, "条目000") {
		t.Errorf("SystemPrompt 不应再读取记忆库:\n%s", got)
	}
}

func TestSystemPromptListsAllEntriesWithinBudget(t *testing.T) {
	p := newTestPlugin(t, nil)
	seed(t, p, 100)

	got := indexPrompt(t, p)
	if !strings.Contains(got, "[已保存的记忆]") {
		t.Fatalf("提示词缺少索引头:\n%s", got)
	}
	// 100 条是本方案的目标规模：必须全部带摘要出现，且不触发任何降级
	for _, i := range []int{0, 1, 42, 99} {
		name := fmt.Sprintf("条目%03d", i)
		if !strings.Contains(got, name) {
			t.Errorf("索引缺少 %s", name)
		}
		if !strings.Contains(got, fmt.Sprintf("这是第 %d 条记忆的摘要说明文字", i)) {
			t.Errorf("索引缺少 %s 的摘要", name)
		}
	}
	if strings.Contains(got, "未列出") {
		t.Error("100 条不应触发截断")
	}
	t.Logf("100 条索引共 %d 字节", len(got))
}

func TestIndexDegradesToTitlesOnly(t *testing.T) {
	p := newTestPlugin(t, nil)
	seed(t, p, 40)

	entries, _ := p.snapshot().store.List()
	full := renderIndex("", entries, 200, 0) // 0 = 不限字节

	// 预算刚好装不下带摘要的版本：应先砍摘要，而不是砍条目
	idx := renderIndex("", entries, 200, len(full)-1)
	if strings.Contains(idx, "摘要说明文字") {
		t.Error("超预算时应先省略摘要")
	}
	if strings.Contains(idx, "未列出") {
		t.Error("砍掉摘要后装得下，就不应再丢条目")
	}
	for i := range 40 {
		if !strings.Contains(idx, fmt.Sprintf("条目%03d", i)) {
			t.Fatalf("降级后仍应列出全部标题，缺少 条目%03d:\n%s", i, idx)
		}
	}
}

func TestIndexTruncatesAndReportsRemainder(t *testing.T) {
	p := newTestPlugin(t, nil)
	seed(t, p, 60)
	entries, _ := p.snapshot().store.List()

	// 条数上限：多出来的应被折成一行提示，而不是静默消失
	idx := renderIndex("", entries, 10, 0)
	if !strings.Contains(idx, "另有 50 条未列出") {
		t.Errorf("应注明被略去的条数:\n%s", idx)
	}
	if n := strings.Count(idx, "\n- "); n != 9 {
		t.Errorf("应列出 10 条，实际 %d 条", n+1)
	}

	// 字节上限极小：仍要给出可继续查询的出口，不能返回空
	tiny := renderIndex("", entries, 200, 120)
	if len(tiny) > 120 && !strings.Contains(tiny, "list_memories") {
		t.Errorf("预算极小时应退回一行说明:\n%s", tiny)
	}
	if tiny == "" {
		t.Error("不应返回空索引")
	}
}

func TestIndexOrderStableAcrossSaves(t *testing.T) {
	p := newTestPlugin(t, nil)
	seed(t, p, 20)
	before := p.SystemPrompt()

	// 更新一条已有记忆：展示顺序按（分类, 标题）固定，不应因时间变化而重排
	s := p.snapshot()
	if _, err := s.store.Save(Entry{
		Name: "条目005", Description: "这是第 5 条记忆的摘要说明文字", Type: Types[5%len(Types)], Content: "新正文",
	}, true); err != nil {
		t.Fatal(err)
	}
	if after := p.SystemPrompt(); after != before {
		t.Errorf("更新正文不应改变索引内容与顺序\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestListMemoriesFilters(t *testing.T) {
	p := newTestPlugin(t, nil)
	s := p.snapshot()
	s.store.Save(Entry{Name: "数据库连接串", Description: "PG 连接参数", Type: "事实", Content: "host=localhost"}, false)
	s.store.Save(Entry{Name: "提交信息用中文", Description: "说明做了什么", Type: "约定", Content: "正文"}, false)
	s.store.Save(Entry{Name: "回答要直接", Description: "不要寒暄", Type: "偏好", Content: "正文"}, false)

	tool := &listTool{p: p}
	run := func(args string) string {
		out, err := tool.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	all := run(`{}`)
	if !strings.Contains(all, "共 3 条记忆") {
		t.Errorf("无过滤时应列出全部:\n%s", all)
	}

	// 关键词命中正文
	byContent := run(`{"keyword":"localhost"}`)
	if !strings.Contains(byContent, "数据库连接串") || strings.Contains(byContent, "回答要直接") {
		t.Errorf("关键词应匹配正文:\n%s", byContent)
	}
	// 关键词大小写不敏感
	if got := run(`{"keyword":"PG"}`); !strings.Contains(got, "数据库连接串") {
		t.Errorf("关键词应匹配摘要:\n%s", got)
	}
	if got := run(`{"keyword":"pg"}`); !strings.Contains(got, "数据库连接串") {
		t.Errorf("关键词匹配应不区分大小写:\n%s", got)
	}
	// 分类过滤
	byType := run(`{"type":"约定"}`)
	if !strings.Contains(byType, "提交信息用中文") || strings.Contains(byType, "数据库连接串") {
		t.Errorf("分类过滤失效:\n%s", byType)
	}
	// 无命中要明确说出来，而不是返回空串让模型以为没有记忆
	if got := run(`{"keyword":"根本不存在的词"}`); !strings.Contains(got, "没有符合条件") {
		t.Errorf("无命中应有明确说明:\n%s", got)
	}
}

func TestRecallMemory(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"max_entry_bytes": 1024})
	p.snapshot().store.Save(Entry{
		Name: "构建命令", Description: "怎么构建", Type: "事实", Content: "go build ./cmd/wen",
	}, false)

	tool := &recallTool{p: p}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"构建命令"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "go build ./cmd/wen") || !strings.Contains(out, "事实/构建命令") {
		t.Errorf("读取结果 = %q", out)
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"没有这条"}`)); err == nil {
		t.Error("读取不存在的记忆应报错")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"  "}`)); err == nil {
		t.Error("空标题应报错")
	}
}

func TestRecallTruncatesLongEntry(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"max_entry_bytes": 1024})
	p.snapshot().store.Save(Entry{
		Name: "长文", Type: "事实", Content: strings.Repeat("很长的正文内容。", 500),
	}, false)

	out, err := (&recallTool{p: p}).Execute(context.Background(), json.RawMessage(`{"name":"长文"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已截断") {
		t.Error("超长记忆应被截断并说明")
	}
	if len(out) > 1024+128 {
		t.Errorf("截断后仍有 %d 字节", len(out))
	}
}

func TestSetConfigTakesEffect(t *testing.T) {
	// 走 Manager 的完整链路：保存配置会重新 Init，不能丢掉记忆目录
	base := t.TempDir()
	m := plugin.NewManager(plugin.InitContext{}, filepath.Join(base, "plugins.state.json"))
	p := New()
	if err := m.Register(p, plugin.PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	p.snapshot().store.Save(Entry{Name: "一条", Description: "钩子", Type: "事实", Content: "正文"}, false)
	if !strings.Contains(indexPrompt(t, p), "一条") {
		t.Fatal("初始状态应能注入索引")
	}

	if err := m.SetConfig("memory", map[string]any{"max_index_entries": 0}); err != nil {
		t.Fatal(err)
	}
	if s := p.snapshot(); s.maxIndexEntries != 0 {
		t.Errorf("配置未生效: %+v", s)
	}
	if s := p.snapshot(); s.store == nil {
		t.Fatal("重新 Init 后记忆目录不应丢失")
	}
	if !strings.Contains(indexPrompt(t, p), "一条") {
		t.Error("改配置后仍应能读到原有记忆")
	}
}

func TestSaveMemoryTool(t *testing.T) {
	p := newTestPlugin(t, nil)
	save := &saveTool{p: p}
	run := func(args string) (string, error) {
		return save.Execute(context.Background(), json.RawMessage(args))
	}

	out, err := run(`{"name":"提交信息用中文","description":"说明做了什么和为什么","type":"约定","content":"正文"}`)
	if err != nil {
		t.Fatal(err)
	}
	// 本轮索引已定型，回显是新记忆在这一轮里唯一的可见途径
	if !strings.Contains(out, "约定/提交信息用中文") || !strings.Contains(out, "说明做了什么和为什么") {
		t.Errorf("保存结果应回显条目: %q", out)
	}
	if !strings.Contains(indexPrompt(t, p), "提交信息用中文") {
		t.Error("保存后应出现在索引里")
	}

	// 默认拒绝覆盖，且要指明该怎么做
	_, err = run(`{"name":"提交信息用中文","description":"另一个说明","type":"约定","content":"新正文"}`)
	if err == nil {
		t.Fatal("默认不应覆盖同名记忆")
	}
	if !strings.Contains(err.Error(), "replace") {
		t.Errorf("拒绝覆盖时应说明如何覆盖: %v", err)
	}

	if _, err := run(`{"name":"提交信息用中文","description":"另一个说明","type":"约定","content":"新正文","mode":"replace"}`); err != nil {
		t.Fatal(err)
	}
	got, _ := p.snapshot().store.Get("提交信息用中文")
	if got.Content != "新正文" {
		t.Errorf("覆盖后内容 = %q", got.Content)
	}

	// 非法输入
	if _, err := run(`{"name":"x","description":"d","type":"约定","content":"  "}`); err == nil {
		t.Error("空内容应被拒绝")
	}
	if _, err := run(`{"name":"x","description":"d","type":"随便","content":"c"}`); err == nil {
		t.Error("未知分类应被拒绝")
	}
	if _, err := run(`不是 JSON`); err == nil {
		t.Error("非法参数应报错")
	}
}

func TestDeleteMemoryTool(t *testing.T) {
	p := newTestPlugin(t, nil)
	p.snapshot().store.Save(Entry{Name: "临时约定", Description: "钩子", Type: "约定", Content: "正文"}, false)

	del := &deleteTool{p: p}
	out, err := del.Execute(context.Background(), json.RawMessage(`{"name":"临时约定"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "约定/临时约定") {
		t.Errorf("删除结果 = %q", out)
	}
	if strings.Contains(indexPrompt(t, p), "临时约定") {
		t.Error("删除后索引里不应再有该条目")
	}
	if _, err := del.Execute(context.Background(), json.RawMessage(`{"name":"临时约定"}`)); err == nil {
		t.Error("重复删除应报错")
	}
}

func TestSaveDeleteVisibleAcrossPluginInstances(t *testing.T) {
	// 记忆的意义在于跨会话可见：换一个插件实例指向同一目录也应读到
	dir := t.TempDir()
	p1 := New()
	if err := p1.Init(plugin.InitContext{StateDir: dir}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := (&saveTool{p: p1}).Execute(context.Background(),
		json.RawMessage(`{"name":"跨会话","description":"钩子","type":"事实","content":"正文"}`)); err != nil {
		t.Fatal(err)
	}

	p2 := New()
	if err := p2.Init(plugin.InitContext{StateDir: dir}, nil); err != nil {
		t.Fatal(err)
	}
	if got := indexPrompt(t, p2); !strings.Contains(got, "跨会话") {
		t.Errorf("新实例应看到已保存的记忆:\n%s", got)
	}
}

func TestToolsRegisterWithoutConflict(t *testing.T) {
	m := plugin.NewManager(plugin.InitContext{}, filepath.Join(t.TempDir(), "s.json"))
	if err := m.Register(New(), plugin.PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range m.EnabledTools() {
		names = append(names, tl.Name())
	}
	want := []string{"save_memory", "recall_memory", "list_memories", "delete_memory"}
	if len(names) != len(want) {
		t.Fatalf("工具列表 = %v", names)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("工具 %d = %q, want %q", i, names[i], n)
		}
	}
}

func TestConfigFieldsRoundTripThroughSettingsPage(t *testing.T) {
	p := New()
	fields := plugin.ConfigFieldsOf(p)

	// 设置页提交的是表单值：数字与开关都以字符串到达，清空的数字框是空串
	cfg, err := plugin.NormalizeConfig(fields, map[string]any{
		"turn_extract":       "true",
		"turn_extract_every": "12",
		"decay":              "true",
		"decay_blur_days":    "",
		"decay_forget_days":  "120",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	s := p.snapshot()
	if !s.turnExtract || s.turnEvery != 12 {
		t.Errorf("定期提炼配置未生效: %+v", s)
	}
	if !s.decay || s.blurDays != defaultBlurDays || s.forgetDays != 120 {
		t.Errorf("淡忘配置未生效（清空的数字框应取默认值）: %+v", s)
	}
}

func TestConfigRejectsIntervalBelowFloor(t *testing.T) {
	fields := plugin.ConfigFieldsOf(New())
	if _, err := plugin.NormalizeConfig(fields, map[string]any{"turn_extract_every": 3}); err == nil {
		t.Error("提炼间隔低于下限时应拒绝")
	}
}

func TestSystemPromptMentionsDecayOnlyWhenEnabled(t *testing.T) {
	off := newPluginWithComplete(t, nil, nil)
	if strings.Contains(off.SystemPrompt(), "decay") {
		t.Error("关闭淡忘时不该注入这段提示词")
	}
	on := newPluginWithComplete(t, nil, decayCfg(30, 90))
	if !strings.Contains(on.SystemPrompt(), "decay") {
		t.Error("开启淡忘后应告诉模型怎么标记")
	}
}

// ---------- 注入的结构 ----------

// 保存判据必须是一个带标题的块。它早先是全篇唯一一段无标题的散文，又排在
// [角色设定 · 最高优先级] 之前，那句「以下设定优先于其它一般性指令」正好把它
// 归进了被压过的那一类——实测同一批对话里 adjust_mood 与 record_touch 都被
// 调用过，save_memory 一次也没有。
func TestGuideIsATitledBlock(t *testing.T) {
	got := newTestPlugin(t, nil).SystemPrompt()
	if !strings.HasPrefix(got, "[长期记忆]\n") {
		t.Errorf("保存判据必须以带标题的块开头，实际开头为:\n%.60s", got)
	}
}

// 判据与索引各占一个标题：一个是「什么该记下来」，一个是「已经记住了什么」。
func TestGuideAndIndexHaveDistinctTitles(t *testing.T) {
	p := newTestPlugin(t, nil)
	seed(t, p, 1)
	guide, index := p.SystemPrompt(), indexPrompt(t, p)
	if strings.Contains(index, "[长期记忆]") {
		t.Error("索引不该复用判据的标题，模型会分不清哪句是清单哪句是判据")
	}
	if strings.Contains(guide, "[已保存的记忆]") {
		t.Error("判据里不该出现索引的标题")
	}
}

// 判据的四类定义必须兼顾生活，不能只认工程语境的东西：早先「事实 = 环境、配置、
// 结构」把「我早上十点上班」这类最常见的记忆排除在外了。
func TestGuideCoversEverydayLife(t *testing.T) {
	got := newTestPlugin(t, nil).SystemPrompt()
	for _, want := range []string{"作息", "家人", "喜欢什么"} {
		if !strings.Contains(got, want) {
			t.Errorf("判据里缺少生活语境的说法 %q", want)
		}
	}
}

// 对方明说「记住」时不该再由模型判断值不值得——这正是报上来的那个场景。
func TestGuideObeysExplicitRememberRequest(t *testing.T) {
	got := newTestPlugin(t, nil).SystemPrompt()
	if !strings.Contains(got, "记住") || !strings.Contains(got, "一律照办") {
		t.Errorf("判据应当明确「对方说记住时一律照办」:\n%s", got)
	}
}
