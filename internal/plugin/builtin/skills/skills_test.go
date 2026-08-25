package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

// 契约检查：设置页的开关、配置、操作与状态行都靠这几个可选接口。
var (
	_ plugin.Plugin         = (*Plugin)(nil)
	_ plugin.Configurable   = (*Plugin)(nil)
	_ plugin.Actionable     = (*Plugin)(nil)
	_ plugin.StatusReporter = (*Plugin)(nil)
	_ plugin.Categorized    = (*Plugin)(nil)
)

// writeSkill 在 dir 下造一个技能。
func writeSkill(t *testing.T, dir, name, front, body string) {
	t.Helper()
	sdir := filepath.Join(dir, name)
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + front + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(sdir, skillFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newAt(t *testing.T, dir string, cfg map[string]any) *Plugin {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: dir}, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p
}

func TestScanLoadsSkills(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "pdf", "name: pdf\ndescription: 处理 PDF 的提取与合并", "第一步：先看页数。")
	writeSkill(t, dir, "xlsx", "description: 读写表格", "用脚本改单元格。")

	res := scan(dir, maxDescRunes)
	if len(res.problems) != 0 {
		t.Fatalf("不该有问题: %v", res.problems)
	}
	if len(res.skills) != 2 {
		t.Fatalf("技能数 = %d", len(res.skills))
	}
	// os.ReadDir 按名字排序，清单因此才可能逐字节稳定
	if res.skills[0].Name != "pdf" || res.skills[1].Name != "xlsx" {
		t.Errorf("顺序不稳定: %v", res.skills)
	}
	if res.skills[0].Desc != "处理 PDF 的提取与合并" {
		t.Errorf("描述 = %q", res.skills[0].Desc)
	}
	if res.skills[0].Dir != filepath.Join(dir, "pdf") {
		t.Errorf("技能目录 = %q", res.skills[0].Dir)
	}
}

func TestScanProblemsDoNotStopTheRest(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "good", "description: 能用的", "正文")
	writeSkill(t, dir, "nodesc", "name: nodesc", "正文") // 没写 description
	os.MkdirAll(filepath.Join(dir, "empty"), 0o755)    // 没有 SKILL.md
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)     // 点开头的目录不是技能
	writeSkill(t, dir, "Bad Name", "description: 名字不合规", "正文")
	// 名称与目录名不一致：仍按目录名加载，只提醒一句
	writeSkill(t, dir, "renamed", "name: 别的名字\ndescription: 改过名的", "正文")

	res := scan(dir, maxDescRunes)
	got := map[string]bool{}
	for _, s := range res.skills {
		got[s.Name] = true
	}
	if !got["good"] || !got["renamed"] {
		t.Fatalf("好的技能没被加载: %v", got)
	}
	if got["nodesc"] || got["empty"] || got[".git"] {
		t.Errorf("不该加载的被加载了: %v", got)
	}
	joined := strings.Join(res.problems, "\n")
	for _, want := range []string{"nodesc", "empty", "Bad Name", "renamed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("问题里缺少 %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, ".git") {
		t.Errorf("点开头的目录不该报成问题: %s", joined)
	}
}

func TestScanMissingDir(t *testing.T) {
	res := scan(filepath.Join(t.TempDir(), "nope"), maxDescRunes)
	if !res.missing {
		t.Fatal("目录不存在应被标出")
	}
	if len(res.skills) != 0 {
		t.Errorf("不该有技能")
	}
}

func TestSplitFrontMatter(t *testing.T) {
	cases := []struct {
		name string
		src  string
		fm   string
		body string
		ok   bool
	}{
		{"常规", "---\na: 1\n---\n正文", "a: 1", "正文", true},
		{"CRLF", "---\r\na: 1\r\n---\r\n正文", "a: 1", "正文", true},
		{"带 BOM", bom + "---\na: 1\n---\n正文", "a: 1", "正文", true},
		{"正文里的分隔线不算结束", "---\na: 1\n---\n正文\n----\n下半段", "a: 1", "正文\n----\n下半段", true},
		{"没有说明块", "直接就是正文", "", "直接就是正文", false},
		{"说明块没闭合", "---\na: 1\n正文", "", "---\na: 1\n正文", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fm, body, ok := splitFrontMatter(c.src)
			if ok != c.ok || fm != c.fm || body != c.body {
				t.Errorf("= %q, %q, %v；想要 %q, %q, %v", fm, body, ok, c.fm, c.body, c.ok)
			}
		})
	}
}

func TestPromptEmptyWhenNoSkills(t *testing.T) {
	p := newAt(t, t.TempDir(), nil)
	if got := p.SystemPrompt(); got != "" {
		t.Errorf("一个技能都没有时不该注入任何东西，却得到 %q", got)
	}
}

func TestPromptCapAndStability(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a1", "a2", "a3", "a4"} {
		writeSkill(t, dir, n, "description: 用途"+n, "正文")
	}
	p := newAt(t, dir, map[string]any{"max_list": 2})

	first := p.SystemPrompt()
	if !strings.Contains(first, "- a1：") || !strings.Contains(first, "- a2：") {
		t.Errorf("前两个应在清单里: %q", first)
	}
	if strings.Contains(first, "- a3：") {
		t.Errorf("超出上限的不该列出: %q", first)
	}
	// 超限时降级要保住「还有东西存在」，否则内容藏住了、存在性也一起没了
	if !strings.Contains(first, "另有 2 个技能未在此列出") {
		t.Errorf("缺少未列出的提示: %q", first)
	}
	// 逐字节一致，否则整段前缀的提示词缓存永远命中不了
	for i := range 3 {
		if got := p.SystemPrompt(); got != first {
			t.Fatalf("第 %d 次返回值变了", i+2)
		}
	}
}

func TestPromptTruncatesLongDescription(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "long", "description: "+strings.Repeat("字", 400), "正文")
	p := newAt(t, dir, nil)
	line := p.SystemPrompt()
	if !strings.Contains(line, "…") {
		t.Errorf("过长的用途应被截断: %q", line)
	}
	if n := strings.Count(line, "字"); n > maxDescRunes {
		t.Errorf("截断后仍超过上限: %d", n)
	}
}

func TestReadSkill(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "pdf", "description: 处理 PDF", "第一步。\n第二步。")
	p := newAt(t, dir, nil)

	out, err := findTool(t, p, "read_skill").Execute(context.Background(),
		mustJSON(t, map[string]string{"name": "pdf"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "第一步。") || !strings.Contains(out, "第二步。") {
		t.Errorf("正文没读全: %q", out)
	}
	if strings.Contains(out, "description:") {
		t.Errorf("开头的说明块不该带进正文: %q", out)
	}
	// 手册里常提到随附文件，模型得知道去哪儿找
	if !strings.Contains(out, filepath.Join(dir, "pdf")) {
		t.Errorf("应给出技能所在目录: %q", out)
	}
}

func TestReadSkillUnknownName(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "pdf", "description: 处理 PDF", "正文")
	p := newAt(t, dir, nil)

	_, err := findTool(t, p, "read_skill").Execute(context.Background(),
		mustJSON(t, map[string]string{"name": "nope"}))
	if err == nil {
		t.Fatal("不存在的技能应报错")
	}
	// 报错要顺带给出可用的名字，否则模型只能反复猜
	if !strings.Contains(err.Error(), "pdf") {
		t.Errorf("报错里应列出已有技能: %v", err)
	}
}

func TestReadSkillTruncates(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "big", "description: 很长", strings.Repeat("字", 2000))
	p := newAt(t, dir, map[string]any{"max_bytes": 1024})
	out, err := findTool(t, p, "read_skill").Execute(context.Background(),
		mustJSON(t, map[string]string{"name": "big"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已截断") {
		t.Errorf("应截断: %q", out)
	}
	if strings.ContainsRune(out, 0xFFFD) {
		t.Fatal("截断切坏了多字节字符")
	}
}

func TestListSkills(t *testing.T) {
	dir := t.TempDir()
	p := newAt(t, dir, nil)

	out, err := findTool(t, p, "list_skills").Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "还没有可用的技能") {
		t.Errorf("空目录的说法不对: %q", out)
	}

	writeSkill(t, dir, "pdf", "description: 处理 PDF", "正文")
	writeSkill(t, dir, "broken", "name: broken", "正文") // 加载不了的
	p = newAt(t, dir, nil)
	out, err = findTool(t, p, "list_skills").Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pdf") {
		t.Errorf("缺少已加载的技能: %q", out)
	}
	// 扫描问题是给用户在设置页修的，模型对它无能为力
	if strings.Contains(out, "broken") {
		t.Errorf("加载失败的不该出现在给模型的清单里: %q", out)
	}
}

func TestInitCreatesDefaultDirAndRejectsNoState(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sub", "skills")
	if err := New().Init(plugin.InitContext{StateDir: base}, nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Errorf("默认目录应被建出来，用户才知道往哪放: %v", err)
	}
	// 没有持久化位置又没指定目录时拒绝启用，不退化到写进程当前目录
	if err := New().Init(plugin.InitContext{}, nil); err == nil {
		t.Error("没有可用目录时应拒绝启用")
	}
}

func TestInitCustomDirNotCreated(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "mine")
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"dir": custom}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 用户指定的目录不代建：路径打错时「目录不存在」才是有用的反馈
	if _, err := os.Stat(custom); !os.IsNotExist(err) {
		t.Error("不该替用户创建他指定的目录")
	}
	if lines := p.StatusLines(); len(lines) != 1 || !strings.Contains(lines[0], "未能加载") {
		t.Errorf("状态行应报出问题: %v", lines)
	}
}

func TestStatusLine(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "pdf", "description: 处理 PDF", "正文")
	lines := newAt(t, dir, nil).StatusLines()
	if len(lines) != 1 {
		t.Fatalf("状态行数 = %d", len(lines))
	}
	if !strings.Contains(lines[0], "1 个") || !strings.Contains(lines[0], dir) {
		t.Errorf("状态行 = %q", lines[0])
	}
}

func TestRescanPicksUpNewSkill(t *testing.T) {
	dir := t.TempDir()
	p := newAt(t, dir, nil)
	if p.SystemPrompt() != "" {
		t.Fatal("初始应为空")
	}

	writeSkill(t, dir, "pdf", "description: 处理 PDF", "正文")
	if err := p.StartAction(context.Background(), actionRescan); err != nil {
		t.Fatal(err)
	}
	st := waitAction(t, p)
	if st.Status != plugin.ActionDone {
		t.Fatalf("状态 = %s，%s", st.Status, st.Message)
	}
	if !strings.Contains(st.Message, "pdf") || !strings.Contains(st.Message, dir) {
		t.Errorf("结果里应有技能名与目录: %q", st.Message)
	}
	// 扫的是当前生效的目录，结果要写回去——不必重启就能用上新放的技能
	if !strings.Contains(p.SystemPrompt(), "pdf") {
		t.Errorf("重新扫描后清单没更新: %q", p.SystemPrompt())
	}
}

func TestRescanDraftDirDoesNotApply(t *testing.T) {
	saved := t.TempDir()
	writeSkill(t, saved, "old", "description: 原来的", "正文")
	p := newAt(t, saved, nil)

	draft := t.TempDir()
	writeSkill(t, draft, "fresh", "description: 试扫的", "正文")
	ctx := plugin.WithActionValues(context.Background(), map[string]any{"dir": draft})
	if err := p.StartAction(ctx, actionRescan); err != nil {
		t.Fatal(err)
	}
	st := waitAction(t, p)
	if !strings.Contains(st.Message, "fresh") {
		t.Errorf("应报告草稿目录里的技能: %q", st.Message)
	}
	if !strings.Contains(st.Message, "尚未保存") {
		t.Errorf("应说明这只是试扫: %q", st.Message)
	}
	// 草稿还不是用户的决定，生效状态不能被它改掉
	if strings.Contains(p.SystemPrompt(), "fresh") || !strings.Contains(p.SystemPrompt(), "old") {
		t.Errorf("试扫不该改动生效的技能: %q", p.SystemPrompt())
	}
}

func TestRescanMissingDirIsAnError(t *testing.T) {
	p := newAt(t, t.TempDir(), nil)
	ctx := plugin.WithActionValues(context.Background(),
		map[string]any{"dir": filepath.Join(t.TempDir(), "nope")})
	if err := p.StartAction(ctx, actionRescan); err != nil {
		t.Fatal(err)
	}
	st := waitAction(t, p)
	if st.Status != plugin.ActionError {
		t.Errorf("目录不存在应报错: %s / %s", st.Status, st.Message)
	}
}

func TestActionsDescribeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	p := newAt(t, dir, nil)
	acts := p.Actions()
	if len(acts) != 1 || acts[0].Key != actionRescan {
		t.Fatalf("操作 = %v", acts)
	}
	// 目录在配置目录深处，不写出来没人找得到。它落在 Detail 而不是 Description：
	// 后者要能进语言包，掺了路径就进不去（见 plugin.ActionDef.Detail）
	if !strings.Contains(acts[0].Detail, dir) {
		t.Errorf("该在 Detail 里给出技能目录: %q", acts[0].Detail)
	}
	if _, err := p.ActionState("nope"); err == nil {
		t.Error("未知操作应报错")
	}
	if err := p.StartAction(context.Background(), "nope"); err == nil {
		t.Error("未知操作应报错")
	}
}

func findTool(t *testing.T, p *Plugin, name string) plugin.Tool {
	t.Helper()
	for _, tl := range p.Tools() {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("找不到工具 %q", name)
	return nil
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func waitAction(t *testing.T, p *Plugin) plugin.ActionState {
	t.Helper()
	for range 200 {
		st, err := p.ActionState(actionRescan)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != plugin.ActionPending {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("操作迟迟没有结束")
	return plugin.ActionState{}
}
