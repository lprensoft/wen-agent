//go:build !windows

package runlock

import "syscall"

// processAlive 报告 pid 对应的进程是否还在。没有权限给它发信号也算「活着」之外的
// 情况：这里只需要一个保守的答案，判不出来就当它已经退出，调用方会退回离线模式。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
