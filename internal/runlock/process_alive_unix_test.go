//go:build !windows

package runlock

import (
	"syscall"
	"testing"
)

// 无权限查询说明进程存在：把它当成已退出会让配置工具绕过正在运行的服务。
func TestAliveFromSignalError(t *testing.T) {
	if !aliveFromSignalError(nil) {
		t.Fatal("成功的 0 号信号被当成了已退出")
	}
	if !aliveFromSignalError(syscall.EPERM) {
		t.Fatal("EPERM 被当成了已退出")
	}
	if aliveFromSignalError(syscall.ESRCH) {
		t.Fatal("ESRCH 被当成了还在运行")
	}
}
