//go:build !windows

package agentplugin

import (
	"errors"
	"os"
	"syscall"
)

func updateLockOpenBusy(error) bool { return false }

func tryUpdateFileLock(f *os.File) (func() error, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errUpdateLockBusy
		}
		return nil, err
	}
	return func() error {
		return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}, nil
}
