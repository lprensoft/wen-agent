//go:build !windows

package runlock

import (
	"errors"
	"syscall"
)

// processAlive 报告 pid 现在是不是还在。
//
// 发 0 号信号只看进程在不在：成功 = 在；ESRCH = 不在了；其余（EPERM 是最常见的）
// = 存在但查不了。后一种不能当成已退出——那会让配置工具绕过正在运行的服务直接写
// 状态文件，身份核实交给 processStart。
func processAlive(pid int) liveness {
	if pid <= 0 {
		return processGone
	}
	return livenessFromSignalError(syscall.Kill(pid, 0))
}

// livenessFromSignalError 是上面那条判据本身，单独拆出来以便在任意平台测。
func livenessFromSignalError(err error) liveness {
	switch {
	case err == nil:
		return processHere
	case errors.Is(err, syscall.ESRCH):
		return processGone
	default:
		return processUnverifiable
	}
}
