package belongings

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func testLimits() Limits { return Limits{MaxContainers: 3, MaxItemsPerContainer: 3} }

func TestStoreAddRemove(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := time.Now()

	// 新建容器并放入
	res, err := s.Apply("冰箱", []Change{{Name: "鸡蛋", Qty: 6}, {Name: "牛奶", Note: "快过期"}}, nil, now, testLimits())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Container != "冰箱" || len(res.Added) != 2 {
		t.Fatalf("回执不对: %+v", res)
	}
	if res.Added[0] != "鸡蛋×6" {
		t.Fatalf("计数物品的回执应带数量: %q", res.Added[0])
	}

	// 消耗一部分
	res, err = s.Apply("冰箱", nil, []Change{{Name: "鸡蛋", Qty: 2}}, now, testLimits())
	if err != nil {
		t.Fatalf("Apply remove: %v", err)
	}
	if res.Removed[0] != "鸡蛋×2（余 4）" {
		t.Fatalf("部分消耗的回执应报余量: %q", res.Removed[0])
	}

	// 减到 0 与整条移除
	if _, err = s.Apply("冰箱", nil, []Change{{Name: "鸡蛋", Qty: 4}, {Name: "牛奶"}}, now, testLimits()); err != nil {
		t.Fatalf("Apply remove all: %v", err)
	}
	cs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(cs) != 1 || len(cs[0].Items) != 0 {
		t.Fatalf("清空后应保留空容器: %+v", cs)
	}
}

func TestStoreMergeSameName(t *testing.T) {
	s := NewStore("", t.TempDir())
	early := time.Now().Add(-72 * time.Hour)
	if _, err := s.Apply("冰箱", []Change{{Name: "鸡蛋", Qty: 4}}, nil, early, testLimits()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	now := time.Now()
	res, err := s.Apply("冰箱", []Change{{Name: "鸡蛋", Qty: 6, Note: "新买的"}}, nil, now, testLimits())
	if err != nil {
		t.Fatalf("Apply merge: %v", err)
	}
	if res.Added[0] != "鸡蛋×6（现共 10）" {
		t.Fatalf("叠加回执不对: %q", res.Added[0])
	}
	cs, _ := s.List()
	it := cs[0].Items[0]
	if it.Qty != 10 || it.Note != "新买的" {
		t.Fatalf("叠加结果不对: %+v", it)
	}
	if !it.Added.Equal(now) {
		t.Fatalf("叠加应把入库时刻更新为最新一次: %v", it.Added)
	}
}

func TestStoreErrors(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := time.Now()
	lim := testLimits()
	if _, err := s.Apply("冰箱", []Change{{Name: "鸡蛋", Qty: 6}}, nil, now, lim); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 取不存在的物品：报错要带现有候选
	_, err := s.Apply("冰箱", nil, []Change{{Name: "苹果"}}, now, lim)
	if err == nil || !strings.Contains(err.Error(), "鸡蛋") {
		t.Fatalf("缺物品的报错应列出现有物品: %v", err)
	}
	// 往不存在的容器里只取不放
	_, err = s.Apply("衣柜", nil, []Change{{Name: "衬衫"}}, now, lim)
	if err == nil || !strings.Contains(err.Error(), "冰箱") {
		t.Fatalf("缺容器的报错应列出现有容器: %v", err)
	}
	// 物品上限：报错要说明规则
	if _, err = s.Apply("冰箱", []Change{{Name: "a"}, {Name: "b"}}, nil, now, lim); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	_, err = s.Apply("冰箱", []Change{{Name: "c"}}, nil, now, lim)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超物品上限的报错应说明规则: %v", err)
	}
	// 容器上限
	for _, name := range []string{"衣柜", "书架"} {
		if _, err := s.Apply(name, []Change{{Name: "x"}}, nil, now, lim); err != nil {
			t.Fatalf("Apply %s: %v", name, err)
		}
	}
	_, err = s.Apply("药箱", []Change{{Name: "创可贴"}}, nil, now, lim)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超容器上限的报错应说明规则: %v", err)
	}
	// 校验失败整批不落盘
	before, _ := s.List()
	_, err = s.Apply("冰箱", []Change{{Name: "苹果", Qty: -1}}, []Change{{Name: "a"}}, now, lim)
	if err == nil {
		t.Fatal("负数数量应报错")
	}
	after, _ := s.List()
	if len(after[containerIndex(after, "冰箱")].Items) != len(before[containerIndex(before, "冰箱")].Items) {
		t.Fatal("整批校验失败时不应有部分生效")
	}
}

func TestStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore("", dir)
	if err := os.WriteFile(filepath.Join(dir, "containers.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("损坏的文件应报错而不是当成空库覆盖")
	}
}

func TestAgeNote(t *testing.T) {
	now := time.Now()
	cases := []struct {
		added time.Time
		want  string
	}{
		{time.Time{}, ""},             // 无时刻不猜
		{now.Add(2 * time.Hour), ""},  // 时钟倒流不猜
		{now.Add(-2 * time.Hour), ""}, // 不满一天不标
		{now.Add(-3 * 24 * time.Hour), "3 天前放入"},
		{now.Add(-40 * 24 * time.Hour), "1 个月前放入"},
		{now.Add(-400 * 24 * time.Hour), "1 年前放入"},
	}
	for _, c := range cases {
		if got := ageNote("", c.added, now); got != c.want {
			t.Errorf("ageNote(%v) = %q, want %q", c.added, got, c.want)
		}
	}
}

func TestRenderDegrade(t *testing.T) {
	now := time.Now()
	// 塞一批长名物品，使「全列」明显长于「只报件数」，降级才有可测的分界
	items := []Item{
		{Name: "鸡蛋", Qty: 4, Added: now.Add(-25 * time.Hour)},
		{Name: "牛奶", Note: "快过期", Added: now},
	}
	for i := range 20 {
		items = append(items, Item{Name: fmt.Sprintf("腌笃鲜的备料第%02d包", i), Added: now})
	}
	cs := []Container{{Name: "冰箱", Items: items}, {Name: "书架"}}

	full := renderContainers("", cs, 8*1024, now)
	if !strings.Contains(full, "鸡蛋×4（1 天前放入）") {
		t.Fatalf("全列应带数量与入库时间: %q", full)
	}
	if !strings.Contains(full, "牛奶（快过期）") {
		t.Fatalf("当天放入的不标时间: %q", full)
	}
	if !strings.Contains(full, "书架：（空）") {
		t.Fatalf("空容器也要占一行（冰箱空了也是信息）: %q", full)
	}

	// 预算装不下全列时，降级为只报件数——容器名与件数必须还在
	mid := renderContainers("", cs, 150, now)
	if !strings.Contains(mid, "冰箱：（22 项") || strings.Contains(mid, "鸡蛋") {
		t.Fatalf("一级降级应只报件数: %q", mid)
	}

	// 连件数都装不下时，只注明容器数
	tiny := renderContainers("", cs, 40, now)
	if !strings.Contains(tiny, "2 个容器") {
		t.Fatalf("兜底降级应注明容器数: %q", tiny)
	}
}

func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p
}

func TestInitRequiresStateDir(t *testing.T) {
	if err := New().Init(plugin.InitContext{}, nil); err == nil {
		t.Fatal("没有持久化目录时应拒绝启用")
	}
}

func TestToolsRoundTrip(t *testing.T) {
	p := newTestPlugin(t)
	ctx := context.Background()
	up := &updateTool{p: p}

	out, err := up.Execute(ctx, json.RawMessage(`{
		"container": "冰箱",
		"add": [{"name": "鸡蛋", "qty": 6}, {"name": "牛奶", "note": "快过期"}],
		"reason": "买菜回来"
	}`))
	if err != nil {
		t.Fatalf("update_items: %v", err)
	}
	if !strings.Contains(out, "入 鸡蛋×6") {
		t.Fatalf("回执应回显放入的物品: %q", out)
	}

	out, err = up.Execute(ctx, json.RawMessage(`{
		"container": "冰箱",
		"remove": [{"name": "鸡蛋", "qty": 2}],
		"reason": "做了番茄炒蛋"
	}`))
	if err != nil {
		t.Fatalf("update_items remove: %v", err)
	}
	if !strings.Contains(out, "余 4") {
		t.Fatalf("消耗回执应报余量: %q", out)
	}

	// 空变化与空缘由都拒绝
	if _, err := up.Execute(ctx, json.RawMessage(`{"container":"冰箱","reason":"x"}`)); err == nil {
		t.Fatal("没有任何变化时应报错")
	}
	if _, err := up.Execute(ctx, json.RawMessage(`{"container":"冰箱","add":[{"name":"x"}],"reason":""}`)); err == nil {
		t.Fatal("缺 reason 时应报错")
	}

	// TurnPrompt 注入当前清单
	tp, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatalf("TurnPrompt: %v", err)
	}
	if !strings.HasPrefix(tp, stateHeaderFor("")) || !strings.Contains(tp, "鸡蛋×4") {
		t.Fatalf("TurnPrompt 应注入清单: %q", tp)
	}

	// list_items 全量与过滤
	ls := &listTool{p: p}
	out, err = ls.Execute(ctx, json.RawMessage(`{"keyword":"牛奶"}`))
	if err != nil {
		t.Fatalf("list_items: %v", err)
	}
	if !strings.Contains(out, "牛奶") || strings.Contains(out, "- 鸡蛋") {
		t.Fatalf("关键词过滤不对: %q", out)
	}
	if _, err := ls.Execute(ctx, json.RawMessage(`{"container":"地窖"}`)); err == nil {
		t.Fatal("不存在的容器应报错并列出现有容器")
	}
}

func TestTurnPromptEmpty(t *testing.T) {
	p := newTestPlugin(t)
	tp, err := p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if err != nil {
		t.Fatalf("TurnPrompt: %v", err)
	}
	if tp != "" {
		t.Fatalf("没有任何容器时不注入: %q", tp)
	}
}

func TestScopeSeparation(t *testing.T) {
	p := newTestPlugin(t)
	up := &updateTool{p: p}

	// 共享域记一个冰箱
	shared := context.Background()
	if _, err := up.Execute(shared, json.RawMessage(`{"container":"冰箱","add":[{"name":"鸡蛋","qty":6}],"reason":"买菜"}`)); err != nil {
		t.Fatalf("共享域写入: %v", err)
	}

	// 某个可见域只读共享域之外自己的库，但共享域（空标签）恒可读
	scoped := plugin.WithScope(context.Background(), plugin.Scope{Write: "inner", Read: []string{}})
	if _, err := up.Execute(scoped, json.RawMessage(`{"container":"衣柜","add":[{"name":"黑裙"}],"reason":"添置"}`)); err != nil {
		t.Fatalf("可见域写入: %v", err)
	}
	tp, err := p.TurnPrompt(scoped, plugin.TurnEvent{})
	if err != nil {
		t.Fatalf("TurnPrompt: %v", err)
	}
	if !strings.Contains(tp, "衣柜") || !strings.Contains(tp, "冰箱") {
		t.Fatalf("可见域应看到自己的库与共享库: %q", tp)
	}

	// 共享域看不到 inner 域的衣柜
	tp, err = p.TurnPrompt(plugin.WithScope(context.Background(), plugin.Scope{Read: []string{}}), plugin.TurnEvent{})
	if err != nil {
		t.Fatalf("TurnPrompt: %v", err)
	}
	if strings.Contains(tp, "衣柜") {
		t.Fatalf("共享域不应看到别的域的容器: %q", tp)
	}

	// 更新已存在于共享域的容器要写回共享域，而不是在本域分叉一份
	if _, err := up.Execute(scoped, json.RawMessage(`{"container":"冰箱","remove":[{"name":"鸡蛋","qty":2}],"reason":"做饭"}`)); err != nil {
		t.Fatalf("跨域更新: %v", err)
	}
	cs, err := p.storeFor("").List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if cs[0].Items[0].Qty != 4 {
		t.Fatalf("更新应落在容器所在的共享库: %+v", cs[0].Items)
	}
}
