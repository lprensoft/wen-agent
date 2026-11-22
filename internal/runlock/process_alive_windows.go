//go:build windows

package runlock

import (
	"errors"

	"golang.org/x/sys/windows"
)

// processAlive 报告 pid 现在是不是还在。
//
// 打不开进程不等于它不在：另一个账号的进程、受保护进程都会返回 AccessDenied，
// 那只能算「查不了」。把它当成已退出会让配置工具绕过正在运行的服务去写状态文件。
func processAlive(pid int) liveness {
	if pid <= 0 {
		return processGone
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// 没有这个 PID 时 OpenProcess 报的是「参数无效」。
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return processGone
		}
		return processUnverifiable
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return processUnverifiable
	}
	const stillActive = 259 // STILL_ACTIVE
	if code == stillActive {
		return processHere
	}
	return processGone
}
