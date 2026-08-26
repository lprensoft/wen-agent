package updater

import (
	"fmt"
	"os"

	"wen/internal/i18n"
)

// Apply 把新二进制换到 exe 的位置。
//
// Windows 不允许删除或覆盖正在运行的可执行文件，**但允许把它改名**——这正是这里
// 三步走的由来：旧的挪开、新的顶上，改名后的旧文件留到下次启动再删（那时它已不再
// 被映射，见 CleanupOld）。
//
// 中途失败要把旧的挪回来：这中间有一小段时间原路径上没有程序，那时断电或被杀软掐掉，
// 用户手上就只剩一个 .old 文件，看不出发生了什么。
func Apply(lang, newBin, exe string) error {
	// 上一次留下的残留可能还在（启动时删不掉的那种），freeBackupPath 会清掉它，
	// 清不掉就往后顺延一个编号
	backup := freeBackupPath(exe)

	if err := os.Rename(exe, backup); err != nil {
		return fmt.Errorf(i18n.T(lang, "updater.err.move_old", "移开旧程序失败: %w"), err)
	}
	if err := os.Rename(newBin, exe); err != nil {
		if rbErr := os.Rename(backup, exe); rbErr != nil {
			return fmt.Errorf(i18n.T(lang, "updater.err.replace_rollback", "替换程序文件失败: %w；旧程序已被移到 %s，请手工改回"), err, backup)
		}
		return fmt.Errorf(i18n.T(lang, "updater.err.replace", "替换程序文件失败: %w"), err)
	}
	return nil
}
