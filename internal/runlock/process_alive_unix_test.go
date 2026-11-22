//go:build !windows

package runlock

import (
	"syscall"
	"testing"
)

// 无权限查询说明进程存在：把它当成已退出会让配置工具绕过正在运行的服务。
func TestLivenessFromSignalError(t *testing.T) {
	if got := livenessFromSignalError(nil); got != processHere {
		t.Fatalf("成功的 0 号信号被判成了 %v", got)
	}
	if got := livenessFromSignalError(syscall.EPERM); got != processUnverifiable {
		t.Fatalf("EPERM 被判成了 %v", got)
	}
	if got := livenessFromSignalError(syscall.ESRCH); got != processGone {
		t.Fatalf("ESRCH 被判成了 %v", got)
	}
}
