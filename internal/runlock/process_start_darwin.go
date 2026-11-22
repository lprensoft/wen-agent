//go:build darwin

package runlock

import (
	"time"

	"golang.org/x/sys/unix"
)

// processStart 返回 pid 的创建时间。查不到返回 false，调用方会退回「身份未确认」。
func processStart(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return time.Time{}, false
	}
	started := info.Proc.P_starttime
	return time.Unix(started.Sec, int64(started.Usec)*1000), true
}
