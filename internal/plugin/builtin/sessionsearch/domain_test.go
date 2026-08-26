package sessionsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// outerScope / innerScope 模仿表里人格的两种可见域。
var (
	outerScope = plugin.Scope{Write: "outer", Read: []string{"outer"}}
	innerScope = plugin.Scope{Write: "inner", Read: []string{"outer", "inner"}}
)

func scoped(sc plugin.Scope) context.Context {
	return plugin.WithScope(context.Background(), sc)
}

func runScoped(t *testing.T, tool plugin.Tool, sc plugin.Scope, args string) string {
	t.Helper()
	out, err := tool.Execute(scoped(sc), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s(%s): %v", tool.Name(), args, err)
	}
	return out
}

// writeTaggedSession 造一个消息带可见域标签的会话，meta 的标签也一并写上。
func writeTaggedSession(t *testing.T, dir, id, title, titleTag string, msgs []session.StoredMessage) {
	t.Helper()
	var b strings.Builder
	meta, _ := json.Marshal(session.Meta{Type: "meta", ID: id, Title: title, Tag: titleTag})
	b.Write(meta)
	b.WriteString("\n")
	for _, m := range msgs {
		line, _ := json.Marshal(m)
		b.Write(line)
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSearchHidesUnreadableMessages(t *testing.T) {
	p, dir := newTestPlugin(t, nil)
	writeTaggedSession(t, dir, "20260817-100000-aaaaaa", "公开的标题", "outer", []session.StoredMessage{
		{Message: llm.Message{Role: llm.RoleUser, Content: "关键词 出现在表侧"}, Tag: "outer", TS: day("2026-08-17 10:00")},
		{Message: llm.Message{Role: llm.RoleUser, Content: "关键词 也出现在里侧"}, Tag: "inner", TS: day("2026-08-17 10:01")},
		{Message: llm.Message{Role: llm.RoleUser, Content: "关键词 还在共享部分"}, TS: day("2026-08-17 10:02")},
	})

	search := &searchTool{p: p}
	outer := runScoped(t, search, outerScope, `{"keyword":"关键词"}`)
	if strings.Contains(outer, "里侧") {
		t.Errorf("表侧不该看到里侧消息:\n%s", outer)
	}
	if !strings.Contains(outer, "表侧") || !strings.Contains(outer, "共享") {
		t.Errorf("表侧应看到自己与共享的消息:\n%s", outer)
	}
	// 命中数按可读消息计，否则数字本身就说明「还有别的内容」
	if !strings.Contains(outer, "命中 2 条") {
		t.Errorf("命中计数应只含可读消息:\n%s", outer)
	}

	inner := runScoped(t, search, innerScope, `{"keyword":"关键词"}`)
	if !strings.Contains(inner, "里侧") || !strings.Contains(inner, "命中 3 条") {
		t.Errorf("里侧应看到全部:\n%s", inner)
	}
}

func TestSearchHidesUnreadableTitle(t *testing.T) {
	p, dir := newTestPlugin(t, nil)
	// 标题取自首条用户消息；这个会话是从里侧开始的，标题因此属于里侧
	writeTaggedSession(t, dir, "20260817-110000-bbbbbb", "里侧才知道的暗号", "inner", []session.StoredMessage{
		{Message: llm.Message{Role: llm.RoleUser, Content: "里侧才知道的暗号"}, Tag: "inner", TS: day("2026-08-17 11:00")},
		{Message: llm.Message{Role: llm.RoleUser, Content: "后来聊起了天气"}, Tag: "outer", TS: day("2026-08-17 11:05")},
	})

	search := &searchTool{p: p}
	outer := runScoped(t, search, outerScope, `{"keyword":"天气"}`)
	if strings.Contains(outer, "暗号") {
		t.Errorf("不可读域的标题不该露出来:\n%s", outer)
	}
	if !strings.Contains(outer, "(无标题)") {
		t.Errorf("标题不可读时应显示占位:\n%s", outer)
	}
	if inner := runScoped(t, search, innerScope, `{"keyword":"天气"}`); !strings.Contains(inner, "暗号") {
		t.Errorf("里侧应看到真实标题:\n%s", inner)
	}
}

func TestReadSessionRespectsScope(t *testing.T) {
	p, dir := newTestPlugin(t, nil)
	writeTaggedSession(t, dir, "20260817-120000-cccccc", "里侧的标题", "inner", []session.StoredMessage{
		{Message: llm.Message{Role: llm.RoleUser, Content: "里侧内容"}, Tag: "inner", TS: day("2026-08-17 12:00")},
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "表侧内容"}, Tag: "outer", TS: day("2026-08-17 12:01")},
	})

	read := &readTool{p: p}
	outer := runScoped(t, read, outerScope, `{"id":"20260817-120000-cccccc"}`)
	if strings.Contains(outer, "里侧内容") || strings.Contains(outer, "里侧的标题") {
		t.Errorf("表侧读到了里侧的内容:\n%s", outer)
	}
	// 条数按可读计
	if !strings.Contains(outer, "共 1 条消息") {
		t.Errorf("消息条数应只含可读的:\n%s", outer)
	}
	if inner := runScoped(t, read, innerScope, `{"id":"20260817-120000-cccccc"}`); !strings.Contains(inner, "共 2 条消息") {
		t.Errorf("里侧应看到全部:\n%s", inner)
	}
}

func TestReadSessionAllUnreadable(t *testing.T) {
	p, dir := newTestPlugin(t, nil)
	writeTaggedSession(t, dir, "20260817-130000-dddddd", "里侧的标题", "inner", []session.StoredMessage{
		{Message: llm.Message{Role: llm.RoleUser, Content: "里侧内容"}, Tag: "inner", TS: day("2026-08-17 13:00")},
	})
	out := runScoped(t, &readTool{p: p}, outerScope, `{"id":"20260817-130000-dddddd"}`)
	if strings.Contains(out, "里侧") {
		t.Errorf("整个会话都不可读时不该露出任何内容:\n%s", out)
	}
	if !strings.Contains(out, "共 0 条消息") {
		t.Errorf("应报告 0 条:\n%s", out)
	}
}

func TestArchivesSplitByDomain(t *testing.T) {
	p := newArchivePlugin(t, nil)
	base := p.snapshot().archiveDir

	inner := sampleEvent()
	inner.Scope = "inner"
	inner.History = []llm.Message{{Role: llm.RoleUser, Content: "里侧的原话"}}
	note, err := p.OnCompact(scoped(innerScope), inner)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimPrefix(note, "（本次压缩前的完整历史已归档："), "inner/") {
		t.Errorf("带域的归档标识应带域前缀: %q", note)
	}

	shared := sampleEvent()
	shared.History = []llm.Message{{Role: llm.RoleUser, Content: "共享的原话"}}
	if _, err := p.OnCompact(scoped(plugin.Scope{}), shared); err != nil {
		t.Fatal(err)
	}

	// 共享域仍落在基准目录（升级前的归档因此照旧可读），里侧落在同级目录
	if n := countMD(t, base); n != 1 {
		t.Errorf("基准目录应只有共享域那 1 份归档，得到 %d", n)
	}
	if n := countMD(t, plugin.DomainDir(base, "inner")); n != 1 {
		t.Errorf("里侧归档应落在 %q，得到 %d 份", plugin.DomainDir(base, "inner"), n)
	}
}

func TestArchiveSearchAndReadRespectScope(t *testing.T) {
	p := newArchivePlugin(t, nil)

	inner := sampleEvent()
	inner.Scope = "inner"
	inner.History = []llm.Message{{Role: llm.RoleUser, Content: "只有里侧说过的暗语"}}
	innerNote, _ := p.OnCompact(scoped(innerScope), inner)
	innerRef := strings.TrimSuffix(strings.TrimPrefix(innerNote, "（本次压缩前的完整历史已归档："), "）")

	shared := sampleEvent()
	shared.SessionID = "20260817-999999-ffffff"
	shared.History = []llm.Message{{Role: llm.RoleUser, Content: "大家都听过的话"}}
	if _, err := p.OnCompact(scoped(plugin.Scope{}), shared); err != nil {
		t.Fatal(err)
	}

	// 检索：表侧查不到里侧归档
	search := &searchTool{p: p}
	outer := runScoped(t, search, outerScope, `{"keyword":"暗语"}`)
	if strings.Contains(outer, "暗语") {
		t.Errorf("表侧不该命中里侧归档:\n%s", outer)
	}
	if got := runScoped(t, search, innerScope, `{"keyword":"暗语"}`); !strings.Contains(got, "暗语") {
		t.Errorf("里侧应命中自己的归档:\n%s", got)
	}

	// 列表：表侧只看到共享那份
	archive := &archiveTool{p: p}
	list := runScoped(t, archive, outerScope, `{}`)
	if strings.Contains(list, "inner/") {
		t.Errorf("表侧的归档列表不该含里侧:\n%s", list)
	}
	if !strings.Contains(list, "共 1 份") {
		t.Errorf("表侧应只看到 1 份归档:\n%s", list)
	}
	if got := runScoped(t, archive, innerScope, `{}`); !strings.Contains(got, "共 2 份") {
		t.Errorf("里侧应看到 2 份:\n%s", got)
	}

	// 按名读取是最直接的越权入口
	args, _ := json.Marshal(map[string]string{"name": innerRef})
	if _, err := archive.Execute(scoped(outerScope), args); err == nil {
		t.Error("表侧按名读取里侧归档应被拒绝")
	}
	if _, err := archive.Execute(scoped(innerScope), args); err != nil {
		t.Errorf("里侧应能读自己的归档: %v", err)
	}
}

func TestParseArchiveRefRejectsTraversal(t *testing.T) {
	for _, bad := range []string{
		"../../etc/passwd", "..\\..\\secret.md", "../x.md",
		"Inner/x.md", "in-ner/x.md", "1abc/x.md", "", "   ",
	} {
		if tag, name, err := parseArchiveRef("", bad); err == nil && (tag != "" || strings.Contains(name, "..")) {
			t.Errorf("%q 应被拒绝，得到 tag=%q name=%q", bad, tag, name)
		}
	}
	// 合法形式
	tag, name, err := parseArchiveRef("", "inner/s1-20260817-101010.md")
	if err != nil || tag != "inner" || name != "s1-20260817-101010.md" {
		t.Errorf("parseArchiveRef 合法输入失败: tag=%q name=%q err=%v", tag, name, err)
	}
	// 共享域的裸文件名，缺后缀时补上
	tag, name, err = parseArchiveRef("", "s1-20260817-101010")
	if err != nil || tag != "" || name != "s1-20260817-101010.md" {
		t.Errorf("共享域标识解析失败: tag=%q name=%q err=%v", tag, name, err)
	}
}

func TestPruneCountsPerDomain(t *testing.T) {
	p := newArchivePlugin(t, map[string]any{"max_archives": 2})

	// 表侧写满上限后再写里侧：里侧的归档不该被表侧挤掉（那是数据丢失，不只是泄漏）
	for i := range 4 {
		ev := sampleEvent()
		ev.Scope = "outer"
		ev.SessionID = fmt.Sprintf("outer-session-%d", i)
		ev.History = []llm.Message{{Role: llm.RoleUser, Content: fmt.Sprintf("表侧第 %d 次", i)}}
		if _, err := p.OnCompact(scoped(outerScope), ev); err != nil {
			t.Fatal(err)
		}
	}
	ev := sampleEvent()
	ev.Scope = "inner"
	ev.History = []llm.Message{{Role: llm.RoleUser, Content: "里侧唯一一份"}}
	if _, err := p.OnCompact(scoped(innerScope), ev); err != nil {
		t.Fatal(err)
	}

	base := p.snapshot().archiveDir
	outerFiles := countMD(t, plugin.DomainDir(base, "outer"))
	innerFiles := countMD(t, plugin.DomainDir(base, "inner"))
	if outerFiles > 2 {
		t.Errorf("表侧归档应被裁到 2 份，得到 %d", outerFiles)
	}
	if innerFiles != 1 {
		t.Errorf("里侧归档应完好保留 1 份，得到 %d", innerFiles)
	}
}

func countMD(t *testing.T, dir string) int {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, de := range des {
		if !de.IsDir() && strings.HasSuffix(de.Name(), ".md") {
			n++
		}
	}
	return n
}

func TestUntaggedBehaviourUnchanged(t *testing.T) {
	// 回归：没有任何裁决插件时（零值 Scope）行为与改动前一致
	p, dir := newTestPlugin(t, nil)
	writeTaggedSession(t, dir, "20260817-140000-eeeeee", "普通会话", "", []session.StoredMessage{
		{Message: llm.Message{Role: llm.RoleUser, Content: "普通内容"}, TS: day("2026-08-17 14:00")},
	})
	out := run(t, &searchTool{p: p}, `{"keyword":"普通"}`)
	if !strings.Contains(out, "普通会话") || !strings.Contains(out, "普通内容") {
		t.Errorf("无标签会话应照常可查:\n%s", out)
	}
}
