//go:build windows

package agentplugin

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestApplyPendingWithoutPendingDoesNotOpenLock(t *testing.T) {
	oldOpen := openUpdateLockFile
	defer func() { openUpdateLockFile = oldOpen }()
	openUpdateLockFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, windows.ERROR_ACCESS_DENIED
	}
	if problems := NewStore("", t.TempDir()).ApplyPending(); len(problems) != 0 {
		t.Fatalf("没有 pending 时不该碰更新锁：%v", problems)
	}
}

func TestApplyPendingBoundsPermanentOpenAccessDenied(t *testing.T) {
	oldOpen, oldTimeout := openUpdateLockFile, updateLockOpenBusyTimeout
	defer func() {
		openUpdateLockFile, updateLockOpenBusyTimeout = oldOpen, oldTimeout
	}()
	openUpdateLockFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, windows.ERROR_ACCESS_DENIED
	}
	updateLockOpenBusyTimeout = 50 * time.Millisecond

	store := NewStore("", t.TempDir())
	if err := os.MkdirAll(store.updatesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.pendingPath("demo"), []byte(`{"name":"demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	problems := store.ApplyPending()
	if len(problems) != 1 {
		t.Fatalf("永久权限错误没有返回诊断：%v", problems)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("权限错误仍在无限重试：%v", elapsed)
	}
}
