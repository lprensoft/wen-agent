package scene

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/plugin"
)

func newTestPlugin(t *testing.T, cfg map[string]any) *Plugin {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p
}

func TestInitRequiresStateDir(t *testing.T) {
	if err := New().Init(plugin.InitContext{}, nil); err == nil {
		t.Fatal("没有 StateDir 时应当拒绝启用")
	}
}

func TestRequiresRoleplay(t *testing.T) {
	got := New().Requires()
	if len(got) != 1 || got[0] != "roleplay" {
		t.Fatalf("Requires = %v, 期望 [roleplay]", got)
	}
}

func TestStoreSaveDupAndReplace(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, err := s.Save(Scene{Name: "老宅阁楼", Detail: "堆满旧木箱，天窗漏光"}, false); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := s.Save(Scene{Name: "巷口面馆", Detail: "只有四张桌子"}, false); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 同名默认拒绝（大小写不敏感）
	if _, err := s.Save(Scene{Name: " 老宅阁楼 ", Detail: "另一份描述"}, false); err == nil {
		t.Fatal("同名场景应当被拒绝")
	}
	// replace 更新描述，保留原位置与创建时间
	first, _ := s.List()
	updated, err := s.Save(Scene{Name: "老宅阁楼", Detail: "天窗换了新玻璃"}, true)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !updated.Created.Equal(first[0].Created) {
		t.Error("replace 应当保留创建时间")
	}
	scenes, _ := s.List()
	if len(scenes) != 2 || scenes[0].Name != "老宅阁楼" || scenes[0].Detail != "天窗换了新玻璃" {
		t.Fatalf("replace 后清单不对: %+v", scenes)
	}
	// 删除
	if _, err := s.Delete("巷口面馆"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if scenes, _ := s.List(); len(scenes) != 1 {
		t.Fatalf("删除后应剩 1 条，得到 %d", len(scenes))
	}
	if _, err := s.Delete("不存在"); err == nil {
		t.Fatal("删除不存在的场景应当报错")
	}
}

func TestStoreValidation(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, err := s.Save(Scene{Name: "", Detail: "x"}, false); err == nil {
		t.Fatal("空名称应当报错")
	}
	if _, err := s.Save(Scene{Name: "x", Detail: "  "}, false); err == nil {
		t.Fatal("空描述应当报错")
	}
	if _, err := s.Save(Scene{Name: strings.Repeat("名", maxNameRunes+1), Detail: "x"}, false); err == nil {
		t.Fatal("超长名称应当报错")
	}
	if _, err := s.Save(Scene{Name: "x", Detail: strings.Repeat("述", maxDetailRunes+1)}, false); err == nil {
		t.Fatal("超长描述应当报错")
	}
	// 名称里的换行与连续空白压成单个空格
	sc, err := s.Save(Scene{Name: "河边\n 凉亭", Detail: "六角亭"}, false)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if sc.Name != "河边 凉亭" {
		t.Fatalf("名称未规整: %q", sc.Name)
	}
}

func TestSystemPrompt(t *testing.T) {
	p := newTestPlugin(t, nil)
	if got := p.SystemPrompt(); !strings.Contains(got, "save_scene") || strings.Contains(got, "[场景与环境]") {
		t.Fatalf("未配置舞台时应只含记录判据: %q", got)
	}
	p2 := newTestPlugin(t, map[string]any{"stage": "民国年间的江南小镇"})
	got := p2.SystemPrompt()
	if !strings.Contains(got, "[场景与环境]") || !strings.Contains(got, "民国年间的江南小镇") {
		t.Fatalf("配置舞台后应注入设定: %q", got)
	}
	if !strings.Contains(got, "save_scene") {
		t.Fatal("记录判据应始终注入")
	}
}

func TestStageClipped(t *testing.T) {
	long := strings.Repeat("景", maxStageBytes) // 3 字节/字，必超
	p := newTestPlugin(t, map[string]any{"stage": long})
	got := p.SystemPrompt()
	if len(got) > maxStageBytes+1024 {
		t.Fatalf("舞台设定未被截断，长度 %d", len(got))
	}
	if !strings.Contains(got, "已截断") {
		t.Fatal("截断时应注明")
	}
}

func TestTurnPromptAndTools(t *testing.T) {
	p := newTestPlugin(t, nil)
	ctx := context.Background()

	// 空库不注入
	if got, err := p.TurnPrompt(ctx, plugin.TurnEvent{}); err != nil || got != "" {
		t.Fatalf("空库应不注入: %q, %v", got, err)
	}

	// 记录一处场景
	save := &saveTool{p: p}
	out, err := save.Execute(ctx, json.RawMessage(`{"name":"巷口面馆","detail":"只有四张桌子，墙上挂着价目表"}`))
	if err != nil {
		t.Fatalf("save_scene: %v", err)
	}
	if !strings.Contains(out, "巷口面馆") {
		t.Fatalf("回显不含场景名: %q", out)
	}
	// 同名再记录被拒
	if _, err := save.Execute(ctx, json.RawMessage(`{"name":"巷口面馆","detail":"x"}`)); err == nil {
		t.Fatal("同名场景应当被拒绝")
	}
	// replace 放行
	if out, err := save.Execute(ctx, json.RawMessage(`{"name":"巷口面馆","detail":"翻新过","mode":"replace"}`)); err != nil || !strings.Contains(out, "已更新") {
		t.Fatalf("replace 应当放行: %q, %v", out, err)
	}

	// TurnPrompt 注入
	got, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil || !strings.Contains(got, "[场景记忆]") || !strings.Contains(got, "巷口面馆") {
		t.Fatalf("TurnPrompt = %q, %v", got, err)
	}

	// list / delete
	list := &listTool{p: p}
	if out, _ := list.Execute(ctx, nil); !strings.Contains(out, "共 1 处场景") {
		t.Fatalf("list_scenes: %q", out)
	}
	if out, _ := list.Execute(ctx, json.RawMessage(`{"keyword":"不存在的词"}`)); !strings.Contains(out, "没有符合条件") {
		t.Fatalf("过滤无命中: %q", out)
	}
	del := &deleteTool{p: p}
	if out, err := del.Execute(ctx, json.RawMessage(`{"name":"巷口面馆"}`)); err != nil || !strings.Contains(out, "已删除") {
		t.Fatalf("delete_scene: %q, %v", out, err)
	}
	if got, _ := p.TurnPrompt(ctx, plugin.TurnEvent{}); got != "" {
		t.Fatalf("删除后应不再注入: %q", got)
	}
}

func TestDomainIsolation(t *testing.T) {
	dir := t.TempDir()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: dir}, nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	save := &saveTool{p: p}

	ctxShared := context.Background()
	ctxA := plugin.WithScope(context.Background(), plugin.Scope{Write: "a", Read: []string{"a"}})
	ctxB := plugin.WithScope(context.Background(), plugin.Scope{Write: "b", Read: []string{"b"}})

	if _, err := save.Execute(ctxShared, json.RawMessage(`{"name":"共享广场","detail":"喷泉"}`)); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := save.Execute(ctxA, json.RawMessage(`{"name":"甲的密室","detail":"暗门"}`)); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 域 a 落在独立目录
	if _, err := NewStore("", filepath.Join(dir, "scenes-a")).List(); err != nil {
		t.Fatalf("读取域 a 的库: %v", err)
	}

	// 域 b 看得到共享，看不到 a；条数也按可读域算
	list := &listTool{p: p}
	out, _ := list.Execute(ctxB, nil)
	if !strings.Contains(out, "共享广场") || strings.Contains(out, "甲的密室") {
		t.Fatalf("域 b 的清单不对: %q", out)
	}
	if !strings.Contains(out, "共 1 处场景") {
		t.Fatalf("条数应按可读域算: %q", out)
	}

	// 域 b 删不动 a 的场景
	del := &deleteTool{p: p}
	if _, err := del.Execute(ctxB, json.RawMessage(`{"name":"甲的密室"}`)); err == nil {
		t.Fatal("不可读域的场景不该删得动")
	}

	// 域 a 看得到自己与共享
	got, err := p.TurnPrompt(ctxA, plugin.TurnEvent{})
	if err != nil || !strings.Contains(got, "甲的密室") || !strings.Contains(got, "共享广场") {
		t.Fatalf("域 a 的注入不对: %q, %v", got, err)
	}
}

func TestRenderScenesDegrade(t *testing.T) {
	scenes := []Scene{
		{Name: "阁楼", Detail: strings.Repeat("长描述", 20)},
		{Name: "面馆", Detail: strings.Repeat("长描述", 20)},
		{Name: "凉亭", Detail: strings.Repeat("长描述", 20)},
	}
	// 预算充足：全列
	full := renderScenes("", scenes, 0, 4096)
	if !strings.Contains(full, "阁楼：") || !strings.Contains(full, "长描述") {
		t.Fatalf("全列不对: %q", full)
	}
	// 预算收紧：省略描述只留名称
	names := renderScenes("", scenes, 0, 60)
	if strings.Contains(names, "长描述") || !strings.Contains(names, "凉亭") {
		t.Fatalf("降级到名称: %q", names)
	}
	// 预算极小：只注明条数
	count := renderScenes("", scenes, 0, 10)
	if !strings.Contains(count, "共 3 处场景") {
		t.Fatalf("最终降级: %q", count)
	}
	// 条数上限：保留最近更新的，注明另有几处
	scenes[0].Updated = scenes[0].Updated.AddDate(0, 0, 1)
	scenes[2].Updated = scenes[2].Updated.AddDate(0, 0, 2)
	limited := renderScenes("", scenes, 2, 4096)
	if strings.Contains(limited, "面馆") || !strings.Contains(limited, "另有 1 处未列出") {
		t.Fatalf("条数上限: %q", limited)
	}
	// 保留者仍按原记录顺序排列
	if strings.Index(limited, "阁楼") > strings.Index(limited, "凉亭") {
		t.Fatalf("展示顺序应保持记录顺序: %q", limited)
	}
}
