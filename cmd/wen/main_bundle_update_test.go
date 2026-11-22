package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/runlock"
)

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
	release, err := runlock.Acquire(base, runlock.Info{PID: 123, Addr: strings.TrimPrefix(ts.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

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
