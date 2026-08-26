//go:build !windows

package updater

import (
	"os"
	"syscall"
)

// Relaunch 用新程序接替当前进程。
//
// Linux 与 macOS 上用 execve 直接换掉进程映像：PID 不变，终端、systemd、launchd
// 那边看不到一次退出，也就不会触发它们各自的重启策略与告警。成功时它不返回。
//
// Go 给自己打开的每个文件描述符都置了 CLOEXEC，监听套接字因此在 exec 的瞬间自动关闭；
// 但调用方仍应先做完优雅关闭——不然正在处理的请求会被硬断。
func Relaunch(lang, exe string, args []string) error {
	argv := append([]string{exe}, args...)
	return syscall.Exec(exe, argv, os.Environ())
}

// RelaunchExits 报告 Relaunch 成功后当前进程是否还需要自行退出。
// 这里是 execve，成功就不再返回，没有「之后」可言。
const RelaunchExits = false
