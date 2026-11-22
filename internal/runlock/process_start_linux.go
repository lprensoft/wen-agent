//go:build linux

package runlock

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// processStart 返回 pid 的创建时间。查不到返回 false，调用方会退回「活着就信」。
func processStart(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	ticks, ok := procStartTicks(data)
	if !ok {
		return time.Time{}, false
	}
	boot, err := bootTime()
	if err != nil {
		return time.Time{}, false
	}
	// /proc 报的时钟单位是 USER_HZ，Linux 上各架构都取 100。
	const userHZ = 100
	return boot.Add(time.Duration(ticks) * (time.Second / userHZ)), true
}

func bootTime() (time.Time, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		sec, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
		if err != nil {
			return time.Time{}, err
		}
		return time.Unix(sec, 0), nil
	}
	return time.Time{}, errors.New("btime 不在 /proc/stat 里")
}
