//go:build !windows && !linux

package runlock

import "time"

// processStart 在这个平台上查不到进程创建时间，调用方退回「活着就信」。宁可保守地
// 认为服务还在运行，也不要因为判不出来而绕过它去直接写状态文件。
func processStart(int) (time.Time, bool) {
	return time.Time{}, false
}
