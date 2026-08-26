//go:build !windows

package updater

import (
	"fmt"
	"os"

	"wen/internal/i18n"
)

// Apply 把新二进制换到 exe 的位置。
//
// Linux 与 macOS 上不能覆盖写一个正在运行的可执行文件，但可以 rename 盖过去：
// 运行中的进程握着的是旧的 inode，它照常跑到退出为止，而路径上已经是新的了。
// rename 本身是原子的，因此不需要先备份再回滚——它要么成了，要么原样没动。
//
// 权限沿用原文件：包里出来的是 0755，而用户可能特意收窄过（比如 0750）。
func Apply(lang, newBin, exe string) error {
	if fi, err := os.Stat(exe); err == nil {
		if err := os.Chmod(newBin, fi.Mode().Perm()); err != nil {
			return fmt.Errorf(i18n.T(lang, "updater.err.chmod", "设置权限失败: %w"), err)
		}
	}
	if err := os.Rename(newBin, exe); err != nil {
		return fmt.Errorf(i18n.T(lang, "updater.err.replace", "替换程序文件失败: %w"), err)
	}
	return nil
}
