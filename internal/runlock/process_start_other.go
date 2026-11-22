//go:build !windows && !linux && !darwin

package runlock

import "time"

// processStart 在这个平台上查不到进程创建时间，调用方会因此把状态判成「无法核实」，
// 由上层决定不写状态文件。
func processStart(int) (time.Time, bool) {
	return time.Time{}, false
}
