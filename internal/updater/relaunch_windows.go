package updater

import (
	"fmt"
	"os"
	"os/exec"

	"wen/internal/i18n"
)

// Relaunch 用新程序接替当前进程。
//
// Windows 没有 exec 这一层，只能起一个新进程再让自己退出。子进程刻意**不加**
// DETACHED_PROCESS / CREATE_NEW_CONSOLE：默认继承父进程的控制台与三个标准流，
// 双击启动时那个窗口于是继续留在原地显示新进程的日志，而不是闪一下换一个新窗口。
//
// 调用方必须在此之前关掉监听端口——父子两个进程会有短暂的重叠，端口没让出来的话
// 新进程起不来。
func Relaunch(lang, exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf(i18n.T(lang, "updater.err.relaunch", "启动新版程序失败: %w"), err)
	}
	return nil
}

// RelaunchExits 报告 Relaunch 成功后当前进程是否还需要自行退出。
// Windows 上新进程已经起来了，这一个必须让位。
const RelaunchExits = true
