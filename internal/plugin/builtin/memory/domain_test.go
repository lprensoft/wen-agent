package memory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/plugin"
)

// outerScope / innerScope 模仿表里人格的两种可见域：
// 前者只读自己与共享，后者两个域都读。
var (
	outerScope = plugin.Scope{Write: "outer", Read: []string{"outer"}}
	innerScope = plugin.Scope{Write: "inner", Read: []string{"outer", "inner"}}
)

func scoped(sc plugin.Scope) context.Context {
	return plugin.WithScope(context.Background(), sc)
}

// saveIn 用 save_memory 在指定可见域下存一条记忆。
func saveIn(t *testing.T, p *Plugin, sc plugin.Scope, name string) {
	t.Helper()
	args := map[string]string{"name": name, "description": "摘要-" + name, "type": "事实", "content": "正文-" + name}
	raw, _ := json.Marshal(args)
	if _, err := (&saveTool{p: p}).Execute(scoped(sc), raw); err != nil {
		t.Fatal(err)
	}
}

func listIn(t *testing.T, p *Plugin, sc plugin.Scope) string {
	t.Helper()
	out, err := (&listTool{p: p}).Execute(scoped(sc), nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDomainDirLayout(t *testing.T) {
	p := newTestPlugin(t, nil)

	base := p.snapshot().store.Dir()
	// 共享域用基准目录本身，已有记忆不需要迁移
	if got := p.storeFor("").Dir(); got != base {
		t.Errorf("共享域目录 = %q, want %q", got, base)
	}
	if got, want := p.storeFor("inner").Dir(), base+"-inner"; got != want {
		t.Errorf("inner 域目录 = %q, want %q", got, want)
	}
	// 同一个域反复取应是同一个 Store 实例（否则每次白丢目录指纹缓存）
	first, second := p.storeFor("inner"), p.storeFor("inner")
	if first != second {
		t.Error("同一域的库应被复用")
	}
}

func TestSaveWritesToWriteDomain(t *testing.T) {
	p := newTestPlugin(t, nil)
	saveIn(t, p, innerScope, "里侧的事")
	saveIn(t, p, outerScope, "表侧的事")

	base := p.snapshot().store.Dir()
	if _, err := p.storeFor("inner").Get("里侧的事"); err != nil {
		t.Errorf("里侧记忆应落在 inner 库: %v", err)
	}
	if _, err := p.storeFor("outer").Get("表侧的事"); err != nil {
		t.Errorf("表侧记忆应落在 outer 库: %v", err)
	}
	// 基准库（共享）不该被这两次写入污染
	if entries, _ := NewStore("", base).List(); len(entries) != 0 {
		t.Errorf("共享库应为空，得到 %d 条", len(entries))
	}
	if !strings.HasSuffix(p.storeFor("inner").Dir(), filepath.Base(base)+"-inner") {
		t.Errorf("目录布局不对: %q", p.storeFor("inner").Dir())
	}
}

func TestListHidesUnreadableDomain(t *testing.T) {
	p := newTestPlugin(t, nil)
	saveIn(t, p, innerScope, "里侧的事")
	saveIn(t, p, outerScope, "表侧的事")

	outer := listIn(t, p, outerScope)
	if strings.Contains(outer, "里侧的事") {
		t.Errorf("表侧不该看到里侧记忆:\n%s", outer)
	}
	if !strings.Contains(outer, "表侧的事") {
		t.Errorf("表侧应看到自己的记忆:\n%s", outer)
	}
	// 条数本身也会泄漏「存在什么」
	if !strings.Contains(outer, "共 1 条记忆") {
		t.Errorf("计数应只含可读域:\n%s", outer)
	}

	inner := listIn(t, p, innerScope)
	if !strings.Contains(inner, "里侧的事") || !strings.Contains(inner, "表侧的事") {
		t.Errorf("里侧应看到全部:\n%s", inner)
	}
	if !strings.Contains(inner, "共 2 条记忆") {
		t.Errorf("里侧计数不对:\n%s", inner)
	}
}

func TestIndexHidesUnreadableDomain(t *testing.T) {
	p := newTestPlugin(t, nil)
	saveIn(t, p, innerScope, "里侧的事")
	saveIn(t, p, outerScope, "表侧的事")

	outer, err := p.TurnPrompt(scoped(outerScope), plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(outer, "里侧的事") {
		t.Errorf("注入的索引泄漏了里侧记忆:\n%s", outer)
	}
	inner, _ := p.TurnPrompt(scoped(innerScope), plugin.TurnEvent{})
	if !strings.Contains(inner, "里侧的事") || !strings.Contains(inner, "表侧的事") {
		t.Errorf("里侧索引应含全部:\n%s", inner)
	}
}

func TestRecallAndDeleteRespectDomain(t *testing.T) {
	p := newTestPlugin(t, nil)
	saveIn(t, p, innerScope, "里侧的事")

	recall := &recallTool{p: p}
	args := json.RawMessage(`{"name":"里侧的事"}`)
	if _, err := recall.Execute(scoped(outerScope), args); err == nil {
		t.Error("表侧不该读到里侧记忆")
	}
	if _, err := recall.Execute(scoped(innerScope), args); err != nil {
		t.Errorf("里侧应能读到自己的记忆: %v", err)
	}

	del := &deleteTool{p: p}
	if _, err := del.Execute(scoped(outerScope), args); err == nil {
		t.Error("表侧不该删得动里侧记忆")
	}
	if _, err := p.storeFor("inner").Get("里侧的事"); err != nil {
		t.Error("越权删除不应真的删掉记忆")
	}
	if _, err := del.Execute(scoped(innerScope), args); err != nil {
		t.Errorf("里侧应能删自己的记忆: %v", err)
	}
}

func TestSaveDuplicateCheckScopedToWriteDomain(t *testing.T) {
	p := newTestPlugin(t, nil)
	saveIn(t, p, innerScope, "同一个标题")

	// 跨域检查同名的话，「已存在同名记忆」这条报错就把里侧的标题吐给了表侧
	raw := json.RawMessage(`{"name":"同一个标题","description":"表侧的","type":"事实","content":"表侧正文"}`)
	out, err := (&saveTool{p: p}).Execute(scoped(outerScope), raw)
	if err != nil {
		t.Fatalf("表侧保存同名记忆不应报错: %v", err)
	}
	if !strings.Contains(out, "同一个标题") {
		t.Errorf("保存结果 = %q", out)
	}
	// 两个域各存一份，互不覆盖
	if e, err := p.storeFor("inner").Get("同一个标题"); err != nil || e.Content != "正文-同一个标题" {
		t.Errorf("里侧记忆被覆盖了: %+v %v", e, err)
	}
}

func TestNameConflictResolvedByWriteDomain(t *testing.T) {
	p := newTestPlugin(t, nil)
	saveIn(t, p, innerScope, "重名")
	saveIn(t, p, outerScope, "重名")

	// 里侧同时能读到两个域的同名记忆，清单里只应出现一条，且落在写入域上
	entries, err := p.visibleEntries(scoped(innerScope))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Name == "重名" {
			n++
			if e.Domain != "inner" {
				t.Errorf("同名冲突应以写入域为准，得到 %q", e.Domain)
			}
		}
	}
	if n != 1 {
		t.Errorf("同名记忆应只出现一次，得到 %d 次", n)
	}
}

func TestSharedDomainReadableFromEverywhere(t *testing.T) {
	p := newTestPlugin(t, nil)
	// 零值 Scope 写入的就是共享库（也是未启用任何裁决插件时的行为）
	saveIn(t, p, plugin.Scope{}, "大家都知道的事")

	for _, sc := range []plugin.Scope{outerScope, innerScope, {}} {
		if !strings.Contains(listIn(t, p, sc), "大家都知道的事") {
			t.Errorf("共享记忆在 %+v 下不可见", sc)
		}
	}
}

func TestUnrestrictedScopeEnumeratesDomains(t *testing.T) {
	p := newTestPlugin(t, nil)
	saveIn(t, p, innerScope, "里侧的事")
	saveIn(t, p, outerScope, "表侧的事")

	// Read 为 nil 表示不限制，此处应枚举出已存在的各个域，与 CanRead 的语义保持一致
	entries, err := p.visibleEntries(scoped(plugin.Scope{}))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["里侧的事"] || !names["表侧的事"] {
		t.Errorf("不限制时应看到全部域: %v", names)
	}
}

func TestExtractWritesToWriteDomain(t *testing.T) {
	c := &fakeComplete{replies: []string{
		`[{"name":"提炼出的事","description":"摘要","type":"事实","content":"正文"}]`,
	}}
	p := newPluginWithComplete(t, c, nil)

	// 压缩时核心按组把该组的可见域放进 ctx，提炼结果应落进那个域
	ctx := scoped(plugin.Scope{Write: "inner", Read: []string{"inner"}})
	if _, err := p.OnCompact(ctx, plugin.CompactEvent{Scope: "inner", History: sampleHistory()}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.storeFor("inner").Get("提炼出的事"); err != nil {
		t.Errorf("提炼结果应落在 inner 库: %v", err)
	}
	if entries, _ := p.storeFor("").List(); len(entries) != 0 {
		t.Errorf("共享库不该被写入: %d 条", len(entries))
	}
}
