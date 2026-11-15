package plugin

import (
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// WriteFileAtomic 把 data 写进同目录的临时文件，再改名替换 path。
//
// 插件状态几乎都是「整份 JSON 读出来、改完写回去」。直接覆盖写一旦在中途出错，留下的
// 就是半个文件，下次连读都读不出来；先写临时文件再改名，读的人只会看到旧版或新版。
//
// Windows 上改名会间歇性失败成 "Access is denied"：杀毒软件与索引器会短暂打开刚写出
// 的临时文件或目标文件，这种占用通常几毫秒就消失。所以在这类平台上重试几次；别的平台
// 改名失败基本都是永久性的（目录没了、跨设备），重试只是白等。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	// CreateTemp 建出来是 0600；调用方要更严的权限时在这里收紧。
	if err := f.Chmod(perm); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := renameWithRetry(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// RenameFile 把 oldpath 改名为 newpath。
//
// 插件状态都是「写临时文件再改名」。Go 的 os.Rename 在 Windows 上会偶发
// "Access is denied"：杀毒软件或索引器短暂打开了刚写出的文件。凡是用 os.Rename
// 替换一个已经存在的状态文件的地方，都应改用它。
func RenameFile(oldpath, newpath string) error {
	return renameWithRetry(oldpath, newpath)
}

// renameWithRetry 在 Windows 上重试改名，接住杀毒软件造成的瞬时占用。
// 最长等约 75ms：够扫完一个几 KB 的 JSON，又不至于把一次保存拖成秒级。
func renameWithRetry(oldpath, newpath string) error {
	err := os.Rename(oldpath, newpath)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	delay := 5 * time.Millisecond
	for i := 0; i < 4; i++ {
		time.Sleep(delay)
		delay *= 2
		if err = os.Rename(oldpath, newpath); err == nil {
			return nil
		}
	}
	return err
}
