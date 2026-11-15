package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomicReplacesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("内容 = %q，期望 new", got)
	}
	// 临时文件不能留在目录里：状态目录会被一遍遍扫描，多出来的文件迟早被误读。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("目录里多出了文件: %v", names)
	}
}

func TestWriteFileAtomicCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFileAtomic(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{}" {
		t.Fatalf("内容 = %q", got)
	}
}

// 写不进去时不能悄悄留下半个文件或临时文件。
func TestWriteFileAtomicFailsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	err := WriteFileAtomic(path, []byte("x"), 0o600)
	if err == nil {
		t.Fatal("目录不存在时应当报错")
	}
	if !strings.Contains(err.Error(), "state.json") {
		t.Errorf("错误里应带上文件名: %v", err)
	}
}
