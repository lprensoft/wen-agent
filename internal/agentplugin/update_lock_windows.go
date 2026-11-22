//go:build windows

package agentplugin

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func updateLockOpenBusy(err error) bool {
	// Windows 在别的句柄持有独占字节范围锁时，连 OpenFile 都可能报
	// AccessDenied 或 SharingViolation，而不一定走到 LockFileEx。
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func tryUpdateFileLock(f *os.File) (func() error, error) {
	overlapped := &windows.Overlapped{}
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errUpdateLockBusy
		}
		return nil, err
	}
	return func() error {
		return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
	}, nil
}
