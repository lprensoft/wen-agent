package health

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/cue"
	"wen/internal/plugin"
)

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func drainCues() { cue.Take(time.Now().Add(365 * 24 * time.Hour)) }

func newTestPlugin(t *testing.T, cfg map[string]any) *Plugin {
	t.Helper()
	p := New()
	p.tick = time.Hour // 测试里不靠定时器，直接调 postDueCues
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return p
}

func call(t *testing.T, p *Plugin, ctx context.Context, name, args string) (string, error) {
	t.Helper()
	for _, tool := range p.Tools() {
		if tool.Name() == name {
			return tool.Execute(ctx, json.RawMessage(args))
		}
	}
	t.Fatalf("没有工具 %s", name)
	return "", nil
}

func TestInitRejectsWithoutStateDir(t *testing.T) {
	if err := New().Init(plugin.InitContext{}, nil); err == nil {
		t.Fatal("没有持久化目录时应当拒绝启用")
	}
}

func TestInitDefaultsReentrantAndStop(t *testing.T) {
	dir := t.TempDir()
	p := New()
	p.tick = time.Millisecond
	if err := p.Init(plugin.InitContext{StateDir: dir}, nil); err != nil {
		t.Fatal(err)
	}
	if s := p.snapshot(); s.cooldown != 7*24*time.Hour || s.maxConditions != 2 || s.maxPeak != 65 {
		t.Fatalf("默认配置 = %+v", s)
	}
	old := p.storeFor("")
	// 重入：先停旧循环再起新的，不会留下两套定时器
	if err := p.Init(plugin.InitContext{StateDir: dir}, map[string]any{
		"cooldown_days": 3, "max_conditions": 1, "max_severity": capSevere,
	}); err != nil {
		t.Fatal(err)
	}
	if p.storeFor("") == old {
		t.Error("重新 Init 后域库表应当被重建")
	}
	if s := p.snapshot(); s.cooldown != 3*24*time.Hour || s.maxConditions != 1 || s.maxPeak != 100 {
		t.Errorf("新配置未生效: %+v", s)
	}
	p.Stop()
	p.Stop() // 重复 Stop 无害
}

func TestSystemPromptStableAndStatic(t *testing.T) {
	p := New() // 未 Init 的插件也会被列表接口调到，不能崩也不能读盘
	a, b := p.SystemPrompt(), p.SystemPrompt()
	if a != b || a == "" {
		t.Fatal("SystemPrompt 应非空且逐字节稳定")
	}
	for _, want := range []string{"set_condition", "update_condition", "adjust_mood", "不演绎需要急救", "没有给出 [身体状况]"} {
		if !strings.Contains(a, want) {
			t.Errorf("system 提示词缺少 %q", want)
		}
	}
	if r := p.Requires(); len(r) != 1 || r[0] != "roleplay" {
		t.Errorf("Requires = %v", r)
	}
	if p.StatusLines("") != nil {
		t.Error("未 Init 不该报状态行")
	}
}

func TestTurnPromptEmptyWhenNothing(t *testing.T) {
	p := newTestPlugin(t, nil)
	if got, err := p.TurnPrompt(context.Background(), plugin.TurnEvent{}); got != "" || err != nil {
		t.Errorf("没有状况不该注入: %q, %v", got, err)
	}
	if p.StatusLines("") != nil {
		t.Error("没有状况不该报状态行")
	}
}

func TestToolsRoundTrip(t *testing.T) {
	p := newTestPlugin(t, nil)
	ctx := context.Background()

	out, err := call(t, p, ctx, "set_condition", `{"name":"着凉","severity":"难受","days":4}`)
	if err != nil || !strings.HasPrefix(out, "已记下：着凉，此刻开始有感觉，最重时难受，硬扛着约 4 天好（眼下硬扛）。") {
		t.Fatalf("登记: %q, %v", out, err)
	}
	tp, _ := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if !strings.HasPrefix(tp, stateHeader+"\n着凉第 1 天，有点不舒服，还在往重里走（硬扛着），预计再四五天好。") || !strings.HasSuffix(tp, guideLine) {
		t.Errorf("注入块不对: %q", tp)
	}
	if lines := p.StatusLines(""); len(lines) != 1 || lines[0] != "🤒 身体：着凉第 1 天（硬扛中）" {
		t.Errorf("状态行 = %q", lines)
	}

	out, err = call(t, p, ctx, "update_condition", `{"care":"吃药"}`)
	if err != nil || !strings.HasPrefix(out, "着凉：处理方式 硬扛→吃药。此刻有点不舒服，") {
		t.Fatalf("改处理方式: %q, %v", out, err)
	}
	if _, err := call(t, p, ctx, "update_condition", `{"name":"发烧","recovered":true}`); err == nil {
		t.Error("不存在的名字应报错")
	}
	// 第二条：超出默认的同时上限 2 之前还能再记一条
	if _, err := call(t, p, ctx, "set_condition", `{"name":"头疼","severity":"有点不舒服","days":1,"onset_hours":2}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, p, ctx, "update_condition", `{"care":"吃药"}`); err == nil || !strings.Contains(err.Error(), "着凉、头疼") {
		t.Errorf("多条时不指名应列候选: %v", err)
	}
	out, err = call(t, p, ctx, "set_condition", `{"name":"发烧","severity":"难受","days":2}`)
	if err == nil || !strings.Contains(err.Error(), "最多记 2 个") {
		t.Errorf("超出同时上限应拒绝并说明: %q, %v", out, err)
	}

	out, err = call(t, p, ctx, "update_condition", `{"name":"头疼","recovered":true}`)
	if err != nil || out != "头疼：已痊愈。" {
		t.Fatalf("痊愈: %q, %v", out, err)
	}
	call(t, p, ctx, "update_condition", `{"name":"着凉","recovered":true}`)
	tp, _ = p.TurnPrompt(ctx, plugin.TurnEvent{})
	if !strings.HasPrefix(tp, stateHeader+"\n刚病好没几天（着凉，刚刚痊愈），还有点虚") {
		t.Errorf("刚痊愈应注入余波: %q", tp)
	}
	// 冷却：拒绝并说明规则
	_, err = call(t, p, ctx, "set_condition", `{"name":"发烧","severity":"难受","days":2}`)
	if err == nil || !strings.Contains(err.Error(), "7 天内不再添新状况") {
		t.Errorf("冷却期内应拒绝并说明: %v", err)
	}
	if p.StatusLines("") != nil {
		t.Error("痊愈后不该报状态行")
	}
}

func TestSeverityCapAndOnsetClip(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"max_severity": capMild})
	ctx := context.Background()
	out, err := call(t, p, ctx, "set_condition", `{"name":"发烧","severity":"病倒了","days":2,"onset_hours":100}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"最重时有点不舒服", "严重度按上限收成「有点不舒服」（原报「病倒了」）", "预计约 2 天后发作", "最多延后 48 小时"} {
		if !strings.Contains(out, want) {
			t.Errorf("回执缺 %q: %q", want, out)
		}
	}
	out, err = call(t, p, ctx, "update_condition", `{"severity":"难受"}`)
	if err != nil || !strings.Contains(out, "按上限收成「有点不舒服」（原报「难受」）") || !strings.Contains(out, "还没发作") {
		t.Errorf("改严重度也受封顶: %q, %v", out, err)
	}
	if _, err := call(t, p, ctx, "set_condition", `{"name":"x","severity":"难受"}`); err == nil {
		t.Error("缺 days 应报错")
	}
	if _, err := call(t, p, ctx, "set_condition", `{bad`); err == nil {
		t.Error("坏 JSON 应报错")
	}
}

func TestDelayedOnsetPostsCueOnce(t *testing.T) {
	drainCues()
	defer drainCues()
	p := newTestPlugin(t, nil)
	store := p.storeFor("")
	now := time.Now()
	c, err := store.Add("着凉", sevModerate, now.Add(3*time.Hour), 4, careTough, now, Limits{})
	if err != nil || c.Cued {
		t.Fatalf("延迟发作的登记不该预先标记已投递: %+v, %v", c, err)
	}

	p.postDueCues(now.Add(time.Hour))
	if cue.Pending(now) {
		t.Fatal("还没到发作时刻不该投递")
	}
	p.postDueCues(now.Add(3*time.Hour + time.Minute))
	got := cue.Take(now)
	if len(got) != 1 || got[0].Source != cueSource || got[0].Key != c.ID {
		t.Fatalf("到点应投递一条: %+v", got)
	}
	if got[0].Text != "先前记下的「着凉」到点发作了——开始有点不舒服，接下来会往难受走。" {
		t.Errorf("理由措辞 = %q", got[0].Text)
	}
	if !got[0].Expire.Equal(c.Onset.Add(cueTTL)) {
		t.Errorf("有效期应为发作后两小时: %v", got[0].Expire)
	}
	// 投过就记在盘上，下一拍不重投
	p.postDueCues(now.Add(4 * time.Hour))
	if cue.Pending(now) {
		t.Error("同一条状况不该重投")
	}
	snap, _ := store.Current(now.Add(4 * time.Hour))
	if !snap.Active[0].Cond.Cued {
		t.Error("投递应落盘")
	}

	// 错过有效期（宕机横跨）：不投，但同样标记，不会在日后补一句「刚开始不舒服」
	c2, _ := store.Add("头疼", sevMild, now.Add(5*time.Hour), 1, careTough, now, Limits{})
	p.postDueCues(now.Add(8 * time.Hour))
	if cue.Pending(now) {
		t.Error("过了有效期不该投")
	}
	snap, _ = store.Current(now.Add(8 * time.Hour))
	for _, v := range snap.Active {
		if v.Cond.ID == c2.ID && !v.Cond.Cued {
			t.Error("错过的也该标记")
		}
	}
}

func TestRecoveredAndClearDropCue(t *testing.T) {
	drainCues()
	defer drainCues()
	p := newTestPlugin(t, nil)
	ctx := context.Background()
	store := p.storeFor("")
	now := time.Now()
	// 发作时刻在一小时前：postDueCues 此刻会投递
	store.Add("着凉", sevModerate, now.Add(-time.Hour), 4, careTough, now.Add(-4*time.Hour), Limits{})
	p.postDueCues(now)
	if !cue.Pending(now) {
		t.Fatal("应先投递")
	}
	if _, err := call(t, p, ctx, "update_condition", `{"recovered":true}`); err != nil {
		t.Fatal(err)
	}
	if cue.Pending(now) {
		t.Error("标记痊愈应撤回还没说出口的理由")
	}

	// 清除操作同样撤回
	store.Add("头疼", sevMild, now.Add(-time.Hour), 1, careTough, now.Add(-30*24*time.Hour), Limits{})
	p.postDueCues(now)
	if !cue.Pending(now) {
		t.Fatal("应先投递")
	}
	if err := p.StartAction(ctx, actionClear); err != nil {
		t.Fatal(err)
	}
	if cue.Pending(now) {
		t.Error("清除应撤回还没说出口的理由")
	}
}

func TestScopeSeparation(t *testing.T) {
	p := newTestPlugin(t, nil)
	inner := plugin.WithScope(context.Background(), plugin.Scope{Write: "inner", Read: []string{}})
	shared := plugin.WithScope(context.Background(), plugin.Scope{Read: []string{}})

	if _, err := call(t, p, inner, "set_condition", `{"name":"着凉","severity":"难受","days":4}`); err != nil {
		t.Fatal(err)
	}
	if tp, _ := p.TurnPrompt(shared, plugin.TurnEvent{}); tp != "" {
		t.Errorf("共享域不该看到 inner 的状况: %q", tp)
	}
	if tp, _ := p.TurnPrompt(inner, plugin.TurnEvent{}); !strings.Contains(tp, "着凉第 1 天") {
		t.Errorf("inner 应看到自己的状况: %q", tp)
	}
	// 共享域的冷却与 inner 无关
	if _, err := call(t, p, shared, "set_condition", `{"name":"头疼","severity":"有点不舒服","days":1}`); err != nil {
		t.Errorf("各域独立: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.snapshot().base+"-inner", "health.json")); err != nil {
		t.Errorf("inner 应落在同级的 -inner 目录: %v", err)
	}
	// 状态行跨全部域，标上域名
	lines := p.StatusLines("")
	if len(lines) != 1 || !strings.Contains(lines[0], "头疼第 1 天（硬扛中）") || !strings.Contains(lines[0], "inner：着凉第 1 天（硬扛中）") {
		t.Errorf("状态行 = %q", lines)
	}
}

func TestClearActionWipesAllDomainsAndCooldown(t *testing.T) {
	p := newTestPlugin(t, nil)
	ctx := context.Background()
	inner := plugin.WithScope(ctx, plugin.Scope{Write: "inner", Read: []string{}})
	call(t, p, ctx, "set_condition", `{"name":"着凉","severity":"难受","days":4}`)
	call(t, p, inner, "set_condition", `{"name":"头疼","severity":"有点不舒服","days":1}`)
	call(t, p, ctx, "update_condition", `{"recovered":true}`) // 共享域进入冷却

	if st, _ := p.ActionState(actionClear); st.Status != plugin.ActionIdle {
		t.Errorf("初始状态 = %+v", st)
	}
	if err := p.StartAction(ctx, actionClear); err != nil {
		t.Fatal(err)
	}
	st, _ := p.ActionState(actionClear)
	if st.Status != plugin.ActionDone || st.Message != "已清除 2 份身体状况记录。" {
		t.Errorf("操作结果 = %+v", st)
	}
	for _, c := range []context.Context{ctx, inner} {
		if tp, _ := p.TurnPrompt(c, plugin.TurnEvent{}); tp != "" {
			t.Errorf("清除后不该再注入: %q", tp)
		}
	}
	// 冷却随之解除
	if _, err := call(t, p, ctx, "set_condition", `{"name":"发烧","severity":"难受","days":2}`); err != nil {
		t.Errorf("清除后冷却应解除: %v", err)
	}
	if err := p.StartAction(ctx, "nope"); err == nil {
		t.Error("未知操作应报错")
	}
	if _, err := p.ActionState("nope"); err == nil {
		t.Error("未知操作应报错")
	}
}
