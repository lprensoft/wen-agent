//go:build !windows

package runlock

import (
	"errors"
	"syscall"
)

// processAlive 报告 pid 现在是否「可能还在运行」。
//
// 发 0 号信号只看进程在不在：成功 = 在；ESRCH = 不在了；EPERM = 在，但不归当前
// 账号。后一种必须算活着——把它当成已退出会让配置工具绕过正在运行的服务去直接写
// 状态文件，而服务端下次全量重写会把那些改动抹掉。身份确认交给 processStart。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return aliveFromSignalError(syscall.Kill(pid, 0))
}

// aliveFromSignalError 是上面那条判据本身，单独拆出来以便在任意平台测。
func aliveFromSignalError(err error) bool {
	return err == nil || errors.Is(err, syscall.EPERM)
}
