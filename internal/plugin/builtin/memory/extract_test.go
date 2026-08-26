package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
)

// fakeComplete 记录收到的提示词并返回预设结果。
type fakeComplete struct {
	replies []string
	calls   int
	prompts []string
	err     error
}

func (f *fakeComplete) fn(_ context.Context, prompt string) (string, error) {
	f.prompts = append(f.prompts, prompt)
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if len(f.replies) == 0 {
		return "[]", nil
	}
	r := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return r, nil
}

func newPluginWithComplete(t *testing.T, c *fakeComplete, cfg map[string]any) *Plugin {
	t.Helper()
	p := New()
	ictx := plugin.InitContext{StateDir: t.TempDir()}
	if c != nil {
		ictx.Complete = c.fn
	}
	if err := p.Init(ictx, cfg); err != nil {
		t.Fatal(err)
	}
	return p
}

func sampleHistory() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleUser, Content: "以后回答别写开场白"},
		{Role: llm.RoleAssistant, Content: "好的"},
	}
}

func TestOnCompactExtractsAndSaves(t *testing.T) {
	c := &fakeComplete{replies: []string{`[
		{"name":"回答不要开场白","description":"直接给结论","type":"偏好","content":"回答时不写开场白。"},
		{"name":"构建命令","description":"go build ./cmd/wen","type":"事实","content":"构建：go build ./cmd/wen"}
	]`}}
	p := newPluginWithComplete(t, c, nil)

	note, err := p.OnCompact(context.Background(), plugin.CompactEvent{
		SessionID: "s1", Summary: "摘要", History: sampleHistory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 {
		t.Errorf("应恰好发起一次提炼调用，实际 %d 次", c.calls)
	}

	// 提炼结果必须真的落盘，而不是只提示模型稍后自己保存
	entries, err := p.snapshot().store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("应保存 2 条记忆，实际 %d 条", len(entries))
	}
	got, err := p.snapshot().store.Get("构建命令")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "事实" || got.Content != "构建：go build ./cmd/wen" {
		t.Errorf("落盘内容不对: %+v", got)
	}

	// 注记落进该会话的摘要，让用户看得见发生了什么
	if !strings.Contains(note, "提炼并保存 2 条") || !strings.Contains(note, "偏好/回答不要开场白") {
		t.Errorf("注记 = %q", note)
	}
	// 历史内容与已有标题都要进提示词
	if !strings.Contains(c.prompts[0], "以后回答别写开场白") {
		t.Error("提示词应包含待提炼的历史")
	}
}

func TestOnCompactSkipsExistingTitles(t *testing.T) {
	c := &fakeComplete{replies: []string{
		`[{"name":"构建命令","description":"新的说法","type":"事实","content":"被改坏的版本"}]`,
	}}
	p := newPluginWithComplete(t, c, nil)
	p.snapshot().store.Save(Entry{Name: "构建命令", Description: "原说法", Type: "事实", Content: "原文"}, false)

	note, err := p.OnCompact(context.Background(), plugin.CompactEvent{SessionID: "s1", History: sampleHistory()})
	if err != nil {
		t.Fatal(err)
	}
	// 压缩摘要里的内容可能已经是残缺副本，覆盖会让原文退化
	got, _ := p.snapshot().store.Get("构建命令")
	if got.Content != "原文" {
		t.Errorf("已有同名记忆不应被覆盖，当前内容 = %q", got.Content)
	}
	if note != "" {
		t.Errorf("没有新增时不应返回注记: %q", note)
	}
	// 已有标题要写进提示词，让模型自己先避开重复
	if !strings.Contains(c.prompts[0], "事实/构建命令") {
		t.Errorf("提示词应列出已有记忆标题:\n%s", c.prompts[0])
	}
}

func TestOnCompactNoCandidates(t *testing.T) {
	c := &fakeComplete{replies: []string{"[]"}}
	p := newPluginWithComplete(t, c, nil)

	note, err := p.OnCompact(context.Background(), plugin.CompactEvent{SessionID: "s1", History: sampleHistory()})
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Errorf("空结果不应返回注记: %q", note)
	}
	if all, _ := p.snapshot().store.List(); len(all) != 0 {
		t.Errorf("不应凑数保存: %d 条", len(all))
	}
}

func TestOnCompactRespectsMaxExtract(t *testing.T) {
	var items []string
	for i := range 10 {
		items = append(items, fmt.Sprintf(
			`{"name":"条目%d","description":"d","type":"事实","content":"c"}`, i))
	}
	c := &fakeComplete{replies: []string{"[" + strings.Join(items, ",") + "]"}}
	p := newPluginWithComplete(t, c, map[string]any{"max_extract": 3})

	if _, err := p.OnCompact(context.Background(), plugin.CompactEvent{SessionID: "s1", History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}
	if all, _ := p.snapshot().store.List(); len(all) != 3 {
		t.Errorf("应只保存 3 条，实际 %d 条", len(all))
	}
}

func TestOnCompactDisabled(t *testing.T) {
	c := &fakeComplete{replies: []string{`[{"name":"x","description":"d","type":"事实","content":"c"}]`}}
	p := newPluginWithComplete(t, c, map[string]any{"auto_extract": false})

	note, err := p.OnCompact(context.Background(), plugin.CompactEvent{SessionID: "s1", History: sampleHistory()})
	if err != nil || note != "" {
		t.Errorf("关闭自动提炼后应直接跳过: note=%q err=%v", note, err)
	}
	if c.calls != 0 {
		t.Error("关闭后不应产生模型调用")
	}
}

func TestOnCompactWithoutModel(t *testing.T) {
	p := newPluginWithComplete(t, nil, nil) // 没有 Complete
	_, err := p.OnCompact(context.Background(), plugin.CompactEvent{SessionID: "s1", History: sampleHistory()})
	if err == nil {
		t.Fatal("没有可用模型时应报错，以便记录日志")
	}
	if !strings.Contains(err.Error(), "模型") {
		t.Errorf("错误信息应说明原因: %v", err)
	}
}

func TestOnCompactModelErrorDoesNotPanic(t *testing.T) {
	c := &fakeComplete{err: fmt.Errorf("上游 429")}
	p := newPluginWithComplete(t, c, nil)
	if _, err := p.OnCompact(context.Background(), plugin.CompactEvent{SessionID: "s1", History: sampleHistory()}); err == nil {
		t.Error("模型调用失败应回报错误（由 Manager 记录日志，不阻断压缩）")
	}
}

func TestOnCompactEmptyHistory(t *testing.T) {
	c := &fakeComplete{}
	p := newPluginWithComplete(t, c, nil)
	note, err := p.OnCompact(context.Background(), plugin.CompactEvent{SessionID: "s1", Summary: "空"})
	if err != nil || note != "" {
		t.Errorf("空历史应跳过: note=%q err=%v", note, err)
	}
	if c.calls != 0 {
		t.Error("空历史不应产生模型调用")
	}
}

func TestParseExtracted(t *testing.T) {
	cases := []struct {
		name, in string
		want     int
	}{
		{"裸数组", `[{"name":"a","description":"d","type":"事实","content":"c"}]`, 1},
		{"围栏包裹", "```json\n[{\"name\":\"a\",\"description\":\"d\",\"type\":\"事实\",\"content\":\"c\"}]\n```", 1},
		{"无语言标记的围栏", "```\n[]\n```", 0},
		{"前后带说明文字", "分析如下：\n[{\"name\":\"a\",\"description\":\"d\",\"type\":\"事实\",\"content\":\"c\"}]\n以上。", 1},
		{"空数组", `[]`, 0},
		{"空串", ``, 0},
		{"对象形式", `{"memories":[{"name":"a","description":"d","type":"事实","content":"c"}],"mentioned":[]}`, 1},
		{"对象形式空 memories", `{"memories":[],"mentioned":["事实/x"]}`, 0},
		{"仅空白", "  \n ", 0},
	}
	for _, c := range cases {
		got, err := parseExtracted(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(got.Memories) != c.want {
			t.Errorf("%s: 解析出 %d 条，want %d", c.name, len(got.Memories), c.want)
		}
	}

	if _, err := parseExtracted("这不是 JSON"); err == nil {
		t.Error("无法解析时应报错")
	}
}

func TestExtractSkipsInvalidItems(t *testing.T) {
	c := &fakeComplete{replies: []string{`[
		{"name":"","description":"d","type":"事实","content":"c"},
		{"name":"没内容","description":"d","type":"事实","content":"   "},
		{"name":"分类不对","description":"d","type":"胡编","content":"c"},
		{"name":"正常条目","description":"d","type":"约定","content":"c"}
	]`}}
	p := newPluginWithComplete(t, c, nil)

	if _, err := p.OnCompact(context.Background(), plugin.CompactEvent{SessionID: "s1", History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}
	all, _ := p.snapshot().store.List()
	if len(all) != 1 || all[0].Name != "正常条目" {
		var names []string
		for _, e := range all {
			names = append(names, e.Name)
		}
		t.Errorf("非法条目应被跳过而不影响其余，实际保存 %v", names)
	}
}

func TestSerializeHistoryTruncates(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: strings.Repeat("很长的提问内容。", 3000)},
	}
	got := serializeHistory("", msgs, 4096)
	if len(got) > 4096+64 {
		t.Errorf("应截断到上限附近，实际 %d 字节", len(got))
	}
	if !strings.Contains(got, "已截断") {
		t.Error("截断后应说明")
	}
}

func TestPluginImplementsCompactObserver(t *testing.T) {
	var _ plugin.CompactObserver = New()

	// 走 Manager 的广播路径（这是核心真正调用的入口）
	c := &fakeComplete{replies: []string{`[{"name":"经由广播","description":"d","type":"事实","content":"c"}]`}}
	base := t.TempDir()
	m := plugin.NewManager(
		plugin.InitContext{Complete: c.fn},
		filepath.Join(base, "plugins.state.json"),
	)
	p := New()
	if err := m.Register(p, plugin.PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	notes := m.NotifyCompact(context.Background(), plugin.CompactEvent{
		SessionID: "s1", History: sampleHistory(),
	})
	if len(notes) != 1 || !strings.Contains(notes[0], "经由广播") {
		t.Errorf("Manager 广播未拿到提炼注记: %v", notes)
	}
}

func TestExtractRevisesContradictedMemory(t *testing.T) {
	c := &fakeComplete{replies: []string{`{"memories":[
		{"name":"饮食禁忌","description":"现在吃香菜","type":"偏好","mode":"replace",
		 "content":"现在吃香菜。此前约两个月一直明确表示不吃。"}
	],"mentioned":[]}`}}
	p := newPluginWithComplete(t, c, nil)

	store := p.snapshot().store
	if _, err := store.Save(Entry{
		Name: "饮食禁忌", Description: "不吃香菜", Type: "偏好", Content: "不吃香菜。",
	}, false); err != nil {
		t.Fatal(err)
	}

	if _, err := p.OnCompact(context.Background(), plugin.CompactEvent{History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}

	// 修订的是同一条，而不是新建出一条与它打架的记忆
	all, _ := store.List()
	if len(all) != 1 {
		t.Fatalf("矛盾应就地修订，实际留下 %d 条", len(all))
	}
	if !strings.Contains(all[0].Content, "现在吃香菜") {
		t.Errorf("未写入新结论：%q", all[0].Content)
	}
	if !strings.Contains(all[0].Content, "此前") {
		t.Errorf("被推翻的旧结论应留在正文里：%q", all[0].Content)
	}
}

func TestExtractSkipsSameNameWithoutReplace(t *testing.T) {
	c := &fakeComplete{replies: []string{
		`[{"name":"饮食禁忌","description":"换个说法","type":"偏好","content":"重复的内容。"}]`,
	}}
	p := newPluginWithComplete(t, c, nil)
	store := p.snapshot().store
	if _, err := store.Save(Entry{
		Name: "饮食禁忌", Description: "不吃香菜", Type: "偏好", Content: "原始内容。",
	}, false); err != nil {
		t.Fatal(err)
	}

	if _, err := p.OnCompact(context.Background(), plugin.CompactEvent{History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}
	e, _ := store.Get("饮食禁忌")
	if e.Content != "原始内容。" {
		t.Errorf("没打算修订就不该覆盖：%q", e.Content)
	}
}

func TestExtractPromptCarriesDescriptions(t *testing.T) {
	c := &fakeComplete{}
	p := newPluginWithComplete(t, c, nil)
	if _, err := p.snapshot().store.Save(Entry{
		Name: "饮食禁忌", Description: "不吃香菜", Type: "偏好", Content: "不吃香菜。",
	}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OnCompact(context.Background(), plugin.CompactEvent{History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}
	// 只给标题的话，模型认不出「饮食禁忌」与「喜欢香菜」说的是同一件事
	if !strings.Contains(c.prompts[0], "偏好/饮食禁忌 — 不吃香菜") {
		t.Errorf("已有记忆清单应带上摘要：\n%s", c.prompts[0])
	}
}

func TestExtractTouchesMentionedMemories(t *testing.T) {
	c := &fakeComplete{replies: []string{`{"memories":[],"mentioned":["事实/老早的事"]}`}}
	p := newPluginWithComplete(t, c, decayCfg(30, 90))
	store := p.snapshot().store
	saveDecaying(t, store, "老早的事", true)
	saveDecaying(t, store, "没提到的事", true)
	backdate(t, store, "老早的事", 60)
	backdate(t, store, "没提到的事", 60)

	if _, err := p.OnCompact(context.Background(), plugin.CompactEvent{History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}
	// 对话里谈到就算用到：不刷新的话，一件一直在聊的事照样会走到淡忘
	if e, _ := store.Get("老早的事"); !sameDay(e.LastUsed, time.Now()) {
		t.Errorf("被提及的记忆应刷新最后使用时间：%v", e.LastUsed)
	}
	if e, _ := store.Get("没提到的事"); sameDay(e.LastUsed, time.Now()) {
		t.Error("没被提及的记忆不该被刷新")
	}
}

func TestExtractPromptMentionsDecayOnlyWhenEnabled(t *testing.T) {
	off := &fakeComplete{}
	p := newPluginWithComplete(t, off, nil)
	if _, err := p.OnCompact(context.Background(), plugin.CompactEvent{History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off.prompts[0], "decay") {
		t.Error("关闭淡忘时不该让模型多做一次无用的判断")
	}

	on := &fakeComplete{}
	q := newPluginWithComplete(t, on, decayCfg(30, 90))
	if _, err := q.OnCompact(context.Background(), plugin.CompactEvent{History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on.prompts[0], "decay") {
		t.Error("开启淡忘后应要求模型给出该字段")
	}
}

// 两份判据必须逐句同源：promptGuideZH 管当场保存、extractPromptZH 管自动提炼，
// 措辞不一致时同一句话会在两条路径上得到相反的取舍——生活类内容曾因此在
// 自动提炼这条主力路径上被窄判据挡在门外（promptGuideZH 修过的问题在这条路径的残留）。
func TestExtractPromptCriteriaMatchesPromptGuide(t *testing.T) {
	defs := []string{
		"偏好：对方明确表达的、长期有效的喜好与要求",
		"约定：两人之间或协作上定下来的规则与安排",
		"事实：关于对方及其处境的、不易重新得知的信息",
		"踩坑：已经验证过的失败原因与正确做法",
		"经历：自己做过、遇到过的事",
		"边界：任何一方明确表达过的不愿意与坚持",
	}
	for _, d := range defs {
		if !strings.Contains(extractPromptZH, d) {
			t.Errorf("extractPromptZH 缺少判据 %q", d)
		}
		if !strings.Contains(promptGuideZH, d) {
			t.Errorf("promptGuideZH 缺少判据 %q（两份判据应同源）", d)
		}
	}
	// 生活语境的关键措辞不得再从提炼路径缺席
	for _, want := range []string{"身份、作息", "喜欢什么、不喜欢什么", "近况"} {
		if !strings.Contains(extractPromptZH, want) {
			t.Errorf("extractPromptZH 缺少生活语境措辞 %q", want)
		}
	}
	// 单次安排归日程这一句同样两边都要有，且措辞一字不差
	const agendaRule = "定在某天某时的单次安排（明天下午三点见面、周六去看展）不存为记忆，归日程，过后由时间线记经历。"
	if !strings.Contains(extractPromptZH, agendaRule) || !strings.Contains(promptGuideZH, agendaRule) {
		t.Errorf("「单次安排归日程」的判据应在两份提示词里措辞一致")
	}
	// 「边界」与「经历」的分界两边都要写出来：一次拒绝完全符合「自己遇到过的事」，
	// 不写分界就会被前一类吸走，连带按生活片段淡忘掉。
	const border = "自己表达的边界归这一类，不归「经历」"
	if !strings.Contains(extractPromptZH, border) || !strings.Contains(promptGuideZH, border) {
		t.Error("「边界」与「经历」的分界应在两份提示词里措辞一致")
	}
	// 旧的纯工程窄措辞不得回归
	for _, bad := range []string{"工作方式或表达方式要求", "项目或协作上确定下来"} {
		if strings.Contains(extractPromptZH, bad) {
			t.Errorf("extractPromptZH 仍含纯工程窄判据 %q", bad)
		}
	}
}

// 「边界」排在 Types 末尾：索引经 TurnPrompt 每轮重发，新类插在中间会让既有分组
// 整体重排，等于让提示词前缀作废一次。
func TestBoundaryTypeSortsLast(t *testing.T) {
	if got := Types[len(Types)-1]; got != typeBoundary {
		t.Errorf("Types 末位 = %q, want %q", got, typeBoundary)
	}
	for _, s := range []string{listSchema(t), saveSchema(t)} {
		if !strings.Contains(s, typeBoundary) {
			t.Errorf("工具 schema 的分类取值缺「%s」：%s", typeBoundary, s)
		}
	}
}

// 自动提炼路径强制边界类不淡忘：立场的改变要有来由、由修订承载，不能由 decay
// 无声发生。其余分类照旧听模型的。
func TestBoundaryTypeNeverDecaysOnExtract(t *testing.T) {
	c := &fakeComplete{replies: []string{
		`[{"name":"不聊他前任","description":"他明确说过不想聊这件事","type":"边界",` +
			`"content":"他说提起前任会不舒服，请不要再问。","decay":true},` +
			`{"name":"这周在搬家","description":"最近忙着搬家","type":"经历",` +
			`"content":"周末两天都在搬东西。","decay":true}]`,
	}}
	p := newPluginWithComplete(t, c, map[string]any{"decay": true})

	if _, err := p.OnCompact(context.Background(), plugin.CompactEvent{History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"不聊他前任": false, "这周在搬家": true}
	for name, wantDecay := range want {
		e, err := p.snapshot().store.Get(name)
		if err != nil {
			t.Fatalf("%s 应已落盘：%v", name, err)
		}
		if e.Decay != wantDecay {
			t.Errorf("%s 的 Decay = %v, want %v", name, e.Decay, wantDecay)
		}
	}
	if !strings.Contains(c.prompts[0], "边界类一律填 false") {
		t.Error("淡忘引导应说明边界类不标淡忘")
	}
}

// listSchema / saveSchema 取两个工具的参数表原文，用来核对分类取值。
func listSchema(t *testing.T) string {
	t.Helper()
	return string((&listTool{p: newPluginWithComplete(t, nil, nil)}).Schema())
}

func saveSchema(t *testing.T) string {
	t.Helper()
	return string((&saveTool{p: newPluginWithComplete(t, nil, nil)}).Schema())
}

// 「经历」是第五类：角色自己做过、遇到过的事。两条路径都得认得它——工具能存，
// 提炼也能挑出来并落盘。
func TestExperienceTypeSavesAndExtracts(t *testing.T) {
	c := &fakeComplete{replies: []string{
		`[{"name":"周末的湖边","description":"和他去湖边散了步","type":"经历","content":"周六下午两个人沿着湖边走了一圈。"}]`,
	}}
	p := newPluginWithComplete(t, c, nil)

	raw, _ := json.Marshal(map[string]string{
		"name": "独自看完的电影", "description": "一个人看了场午夜场", "type": "经历", "content": "片尾曲放完才走。",
	})
	if _, err := (&saveTool{p: p}).Execute(context.Background(), raw); err != nil {
		t.Fatalf("save_memory 应接受「经历」：%v", err)
	}
	if _, err := p.OnCompact(context.Background(), plugin.CompactEvent{History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"独自看完的电影", "周末的湖边"} {
		e, err := p.snapshot().store.Get(name)
		if err != nil {
			t.Fatalf("%s 应已落盘：%v", name, err)
		}
		if e.Type != "经历" {
			t.Errorf("%s 的分类 = %q, want 经历", name, e.Type)
		}
	}
	if !strings.Contains(c.prompts[0], "经历：") || !strings.Contains(c.prompts[0], "踩坑|经历") {
		t.Error("提炼提示词应含「经历」的判据与取值")
	}
}
