package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/runlock"
)

// startForeignProcess 起一个会睡一会儿的子进程，当作「另一个活着的实例」。
func startForeignProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestForeignProcessHelper")
	cmd.Env = append(os.Environ(), "WEN_TEST_FOREIGN_PROCESS=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

// TestForeignProcessHelper 不是测试：被 startForeignProcess 拉起来时只负责活着。
func TestForeignProcessHelper(t *testing.T) {
	if os.Getenv("WEN_TEST_FOREIGN_PROCESS") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

func TestApplyPendingForServeRefusesLiveInstance(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/status" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	base := t.TempDir()
	inst, err := runlock.Lock(base)
	if err != nil {
		t.Fatal(err)
	}
	// 登记必须看起来像另一个活着的进程：PID 得在，且创建时间早于登记时间。
	if err := inst.Register(runlock.Info{
		PID:     startForeignProcess(t),
		Addr:    strings.TrimPrefix(ts.URL, "http://"),
		Started: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	defer inst.Release()

	formal := filepath.Join(base, "bundles", "demo")
	if err := os.MkdirAll(formal, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := []byte("old")
	if err := os.WriteFile(filepath.Join(formal, "plugin.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	updates := filepath.Join(base, "bundles", ".updates")
	if err := os.MkdirAll(updates, 0o755); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(updates, "pending-demo.json")
	if err := os.WriteFile(pending, []byte(`{"name":"demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := applyPendingForServe(base); err == nil {
		t.Fatal("已有存活实例时不该应用待更新包")
	}
	if body, _ := os.ReadFile(filepath.Join(formal, "plugin.json")); string(body) != "old" {
		t.Fatal("第二实例改动了正在使用的插件目录")
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatal("第二实例清掉了待更新记录")
	}
}
