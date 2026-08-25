package bundles

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

// 契约检查：设置页上这张卡片全靠这几个可选接口。
var (
	_ plugin.Plugin         = (*Plugin)(nil)
	_ plugin.Configurable   = (*Plugin)(nil)
	_ plugin.Actionable     = (*Plugin)(nil)
	_ plugin.StatusReporter = (*Plugin)(nil)
	_ plugin.Categorized    = (*Plugin)(nil)
)

const manifest = `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "demo-pack",
  "version": "1.0",
  "description": "for tests"
}`

// writeBundle 造一个只带技能的插件包。
func writeBundle(t *testing.T, dir string) string {
	t.Helper()
	skill := filepath.Join(dir, "skills", "thing")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: thing\ndescription: 做那件事\n---\n正文"
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newAt(t *testing.T, baseDir string) *Plugin {
	t.Helper()
	p := New(baseDir)
	if err := p.Init(plugin.InitContext{}, nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p
}

// 等操作跑完。长流程在后台 goroutine 里，界面靠轮询，这里也一样。
func waitAction(t *testing.T, p *Plugin, key string) plugin.ActionState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.ActionState(key)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != plugin.ActionPending {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("操作一直没结束")
	return plugin.ActionState{}
}

// 装一个包：路径取自配置弹窗里**尚未保存**的草稿值，用户填完不必先保存就能装。
func TestInstallFromDraftValue(t *testing.T) {
	base := t.TempDir()
	src := writeBundle(t, filepath.Join(base, "src"))
	p := newAt(t, filepath.Join(base, "cfg"))

	ctx := plugin.WithActionValues(context.Background(), map[string]any{"source": src})
	if err := p.StartAction(ctx, actionInstall); err != nil {
		t.Fatalf("StartAction: %v", err)
	}
	st := waitAction(t, p, actionInstall)
	if st.Status != plugin.ActionDone {
		t.Fatalf("装失败了: %+v", st)
	}
	if !strings.Contains(st.Message, "demo-pack") {
		t.Errorf("该说清装的是哪个: %q", st.Message)
	}
	// 「装好了」与「用得上了」不是一回事：插件在启动期注册，不说的话用户会盯着
	// 没有新卡片的页面以为装失败了
	if !strings.Contains(st.Message, "重启") {
		t.Errorf("该说清要重启: %q", st.Message)
	}
	if lines := p.StatusLines(""); len(lines) != 1 || !strings.Contains(lines[0], "1 个") {
		t.Errorf("状态行不对: %v", lines)
	}
}

// 什么都没填就点安装：说清楚要先填，而不是报一句底层的路径错误。
func TestInstallWithoutSource(t *testing.T) {
	p := newAt(t, t.TempDir())
	if err := p.StartAction(context.Background(), actionInstall); err == nil {
		t.Fatal("没填路径却开始装了")
	}
	st, _ := p.ActionState(actionInstall)
	if st.Status != plugin.ActionError || !strings.Contains(st.Message, "填") {
		t.Errorf("该提示先填路径: %+v", st)
	}
}

func TestRefreshWithNothingInstalled(t *testing.T) {
	p := newAt(t, t.TempDir())
	if err := p.StartAction(context.Background(), actionRefresh); err != nil {
		t.Fatalf("StartAction: %v", err)
	}
	st := waitAction(t, p, actionRefresh)
	if st.Status != plugin.ActionDone || !strings.Contains(st.Message, "一个") {
		t.Errorf("没装过东西时该好好说一句: %+v", st)
	}
}

// 没有可用的配置目录时拒绝启用，而不是退化到往进程当前目录里写。
func TestInitWithoutBaseDir(t *testing.T) {
	if err := New("").Init(plugin.InitContext{}, nil); err == nil {
		t.Fatal("没有配置目录却启用成功了")
	}
}

func TestActionsAlwaysCheap(t *testing.T) {
	p := newAt(t, t.TempDir())
	acts := p.Actions()
	if len(acts) != 2 {
		t.Fatalf("该有安装与重新握手两个操作: %+v", acts)
	}
	if _, err := p.ActionState("nope"); err == nil {
		t.Error("不认识的操作该报错")
	}
}
