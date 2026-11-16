package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 写完之后读到的必须是新内容，且目录里不留临时文件。
func TestWriteFileReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("内容 = %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("留下了临时文件：%s", e.Name())
		}
	}
}

// 权限按调用方给的收紧，不沿用临时文件的 0600。Windows 只认只读位，跳过。
func TestWriteFileAppliesPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不保留 Unix 权限位")
	}
	path := filepath.Join(t.TempDir(), "secret.json")
	if err := WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("权限 = %v", info.Mode().Perm())
	}
}

// 目录不存在时不自己造：该由调用方决定要不要建。
func TestWriteFileMissingDirFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "state.json")
	if err := WriteFile(path, []byte("x"), 0o600); err == nil {
		t.Fatal("目录不存在时应当报错")
	}
}

// 目标被短暂占用时改名要重试，而不是立刻失败。Windows 上打开的文件会挡住改名；
// 别的平台上打开的文件不挡，这个用例只是一次普通改名。
func TestRenameRetriesWhileTargetIsBusy(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a.tmp"), filepath.Join(dir, "a.json")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = f.Close()
	}()
	if err := Rename(src, dst); err != nil {
		t.Fatalf("占用消失后应当改名成功：%v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new" {
		t.Fatalf("内容 = %q", got)
	}
}

// 改名失败时不能把临时文件留在目录里。
func TestWriteFileCleansTempOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path, 0o755); err != nil { // 目标是个目录，改名必然失败
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("x"), 0o600); err == nil {
		t.Fatal("目标是目录时应当报错")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("失败后留下了临时文件：%s", e.Name())
		}
	}
}

func TestRenameMoves(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a.tmp"), filepath.Join(dir, "a.json")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("源文件还在：%v", err)
	}
}
