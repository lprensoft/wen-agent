package runlock

import (
	"errors"
	"os"
	"testing"
)

func TestAcquireIsExclusivePerDirectory(t *testing.T) {
	dir := t.TempDir()
	release, err := Acquire(dir, Info{PID: 1, Addr: "127.0.0.1:8090"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, Info{PID: 2, Addr: "127.0.0.1:8091"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("第二个实例没有被拦住：%v", err)
	}
	info, ok := Read(dir)
	if !ok || info.Addr != "127.0.0.1:8090" {
		t.Fatalf("第二个实例改写了登记：%+v %v", info, ok)
	}

	release()
	release() // 重复调用不该出问题
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("释放后登记文件还在：%v", err)
	}
	again, err := Acquire(dir, Info{PID: 3, Addr: "127.0.0.1:8091"})
	if err != nil {
		t.Fatalf("前一个实例退出后仍取不到运行权：%v", err)
	}
	defer again()
}
