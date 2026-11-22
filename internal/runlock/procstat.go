package runlock

import (
	"bytes"
	"strconv"
	"strings"
)

// procStartTicks 从 /proc/<pid>/stat 的内容里取出 starttime：自开机起的时钟滴答数。
//
// comm 放在括号里，可能含空格与括号（进程名可以随便改），所以从**最后一个** ')' 之后
// 开始切字段，不能按整行切。取完字段后 starttime 是从 state 数起的第 20 个。
func procStartTicks(stat []byte) (uint64, bool) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 || end+2 >= len(stat) {
		return 0, false
	}
	fields := strings.Fields(string(stat[end+2:]))
	const starttime = 19
	if len(fields) <= starttime {
		return 0, false
	}
	ticks, err := strconv.ParseUint(fields[starttime], 10, 64)
	if err != nil {
		return 0, false
	}
	return ticks, true
}
