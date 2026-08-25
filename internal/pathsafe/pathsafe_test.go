package pathsafe

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWithinPlainPaths(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if !Within(root, sub) {
		t.Error("普通的子目录该在里面")
	}
	if !Within(root, root) {
		t.Error("根自己该算在里面")
	}
	outside := t.TempDir()
	if Within(root, outside) {
		t.Error("另一个目录不该算在里面")
	}
}

// 路径不存在、解析不了时一律按「不在里面」处理——拿不准的时候拒绝。
func TestWithinMissingPathIsRefused(t *testing.T) {
	root := t.TempDir()
	if Within(root, filepath.Join(root, "nope")) {
		t.Error("不存在的路径不该算在里面")
	}
	if Within(filepath.Join(root, "nope"), root) {
		t.Error("根不存在时不该算在里面")
	}
}

// 这是本包存在的理由：Windows 上的目录联接不被 filepath.EvalSymlinks 跟随，
// 只用它做的越界检查在 Windows 上等于没做。
func TestWithinFollowsLinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside", "target")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	escaping := filepath.Join(root, "escaping")
	linkDir(t, outside, escaping)
	if Within(root, escaping) {
		t.Error("经链接指到外面的路径该被判成越界")
	}

	inside := filepath.Join(root, "real")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	linkDir(t, inside, alias)
	if !Within(root, alias) {
		t.Error("指到根内的链接该照常算在里面")
	}
}

// linkDir 建一条指向目录的链接。
//
// Windows 上普通用户建不了符号链接（要「创建符号链接」这项权限），但建得了目录联接，
// 而这两者正是本包要区别对待的东西——所以这里退回联接而不是直接跳过整条用例。
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS != "windows" {
		t.Skip("这个环境建不了指向目录的链接")
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("这个环境建不了目录联接：%v（%s）", err, out)
	}
}
