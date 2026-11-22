package runlock

import (
	"errors"
	"os"
	"testing"
)

func TestLockIsExclusivePerDirectory(t *testing.T) {
	dir := t.TempDir()
	inst, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Register(Info{PID: 1, Addr: "127.0.0.1:8090"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("第二个实例没有被拦住：%v", err)
	}
	info, ok := Read(dir)
	if !ok || info.Addr != "127.0.0.1:8090" {
		t.Fatalf("第二个实例改写了登记：%+v %v", info, ok)
	}

	inst.Release()
	inst.Release() // 重复调用不该出问题
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("释放后登记文件还在：%v", err)
	}
	again, err := Lock(dir)
	if err != nil {
		t.Fatalf("前一个实例退出后仍取不到运行权：%v", err)
	}
	defer again.Release()
}

// 拒绝启动时还没有 Register 过，Release 不能把仍在运行的旧实例的登记抹掉：否则
// 连着启动两次就查不到它了。
func TestReleaseKeepsRegistrationItNeverWrote(t *testing.T) {
	dir := t.TempDir()
	old := []byte(`{"pid":123,"addr":"127.0.0.1:8090"}`)
	if err := os.WriteFile(Path(dir), old, 0o644); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		inst, err := Lock(dir)
		if err != nil {
			t.Fatalf("第 %d 次取锁失败：%v", attempt, err)
		}
		inst.Release()
		info, ok := Read(dir)
		if !ok || info.PID != 123 {
			t.Fatalf("第 %d 次退出抹掉了旧实例的登记：%+v %v", attempt, info, ok)
		}
	}
}

// 旧版服务只写登记、不持运行权：新版取到锁之后、Register 之前，必须还能读到它的
// 地址，否则 startup 无法发现它仍在运行。
func TestLockKeepsExistingRegistrationUntilRegister(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(`{"pid":123,"addr":"127.0.0.1:8090"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	inst, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Release()
	if info, ok := Read(dir); !ok || info.Addr != "127.0.0.1:8090" {
		t.Fatalf("取锁时覆盖了已有登记：%+v %v", info, ok)
	}
	if err := inst.Register(Info{PID: 4, Addr: "127.0.0.1:8091"}); err != nil {
		t.Fatal(err)
	}
	if info, _ := Read(dir); info.PID != 4 {
		t.Fatalf("登记没有写进去：%+v", info)
	}
}
