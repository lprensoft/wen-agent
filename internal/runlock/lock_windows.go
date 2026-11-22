//go:build windows

package runlock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

var errLockBusy = errors.New("运行权已被占用")

// lockOpenBusy 判断打开锁文件失败是不是「另一个实例正持着它」：Windows 在别的
// 句柄持有独占字节范围锁时，OpenFile 报的通常是 AccessDenied 或 SharingViolation，
// 而不一定走到 LockFileEx。
func lockOpenBusy(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// lockExclusive 在已经打开的锁文件上取一把非阻塞的独占锁。出错时区分「被占用」
// 与真正的故障：前者该让第二个实例退出，后者只该提示一句。
func lockExclusive(f *os.File) (func() error, error) {
	overlapped := &windows.Overlapped{}
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLockBusy
		}
		return nil, err
	}
	return func() error {
		return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
	}, nil
}
