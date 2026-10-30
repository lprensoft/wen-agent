package imbot

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPushTargetStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadPushTargets(dir, "tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Get("u1"); got != "" {
		t.Fatalf("空存储里读到 %q", got)
	}
	if err := store.Set("u1", "ctx-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("u2", "ctx-2"); err != nil {
		t.Fatal(err)
	}
	if got := store.Get("u1"); got != "ctx-1" {
		t.Errorf("u1 = %q, 想要 ctx-1", got)
	}

	path := filepath.Join(dir, "tokens.json")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("凭据文件对同组/其他用户可访问: %o", perm)
		}
	}

	reloaded, err := LoadPushTargets(dir, "tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get("u1"); got != "ctx-1" {
		t.Errorf("重启后 u1 = %q, 想要 ctx-1", got)
	}
	if got := reloaded.Get("u2"); got != "ctx-2" {
		t.Errorf("重启后 u2 = %q, 想要 ctx-2", got)
	}
}

func TestPushTargetStoreEmptyValuesAreNoOps(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadPushTargets(dir, "tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("", "ctx"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("u1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tokens.json")); !os.IsNotExist(err) {
		t.Errorf("空值不该落盘: %v", err)
	}
}

func TestPushTargetStoreRejectsInvalidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPushTargets(dir, "tokens.json"); err == nil {
		t.Error("损坏的凭据文件应返回错误")
	}
}

func TestPushTargetStoreHandlesNullFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := LoadPushTargets(dir, "tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Get("u1"); got != "" {
		t.Errorf("null 文件里读到 %q", got)
	}
	if err := store.Set("u1", "ctx-1"); err != nil {
		t.Fatal(err)
	}
	if got := store.Get("u1"); got != "ctx-1" {
		t.Errorf("写入后 u1 = %q, 想要 ctx-1", got)
	}
}
