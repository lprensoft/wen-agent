//go:build !windows

package runlock

import (
	"errors"
	"os"
	"syscall"
)

var errLockBusy = errors.New("运行权已被占用")

func lockOpenBusy(error) bool { return false }

// lockExclusive 在已经打开的锁文件上取一把非阻塞的独占锁。出错时区分「被占用」
// 与真正的故障：前者该让第二个实例退出，后者只该提示一句。
func lockExclusive(f *os.File) (func() error, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errLockBusy
		}
		return nil, err
	}
	return func() error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
