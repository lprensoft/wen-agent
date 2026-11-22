//go:build windows

package runlock

import "golang.org/x/sys/windows"

// processAlive 报告 pid 对应的进程是否还在。
//
// Windows 上 os.FindProcess 会真的 OpenProcess，进程不存在就报错；这里再读一次退出
// 码，避免拿到一个已经结束但句柄还开着的进程。打开失败（不存在、没有权限）一律当
// 作已退出：判不出来时退回离线模式，比连到别人的实例上安全。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}
