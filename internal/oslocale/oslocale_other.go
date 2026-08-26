//go:build !windows

package oslocale

import "os"

// Current 从环境变量获取区域设置（如 zh_CN.UTF-8）。
func Current() string {
	for _, key := range []string{"LC_ALL", "LANG"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return ""
}
