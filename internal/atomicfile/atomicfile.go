// Package atomicfile 把「先写临时文件、再改名替换」这件事收在一处。
//
// 配置、会话与插件状态几乎都是「整份读出来、改完写回去」。直接覆盖写一旦在中途
// 出错，留下的就是半个文件，下次连读都读不出来；先写临时文件再改名，读的人只会
// 看到旧版或新版。
//
// Windows 上改名会间歇性失败成 "Access is denied"：杀毒软件与索引器会短暂打开
// 刚写出的临时文件或目标文件，这种占用通常几毫秒就消失。所以在这类平台上重试
// 几次；别的平台改名失败基本都是永久性的（目录没了、跨设备），重试只是白等。
package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// WriteFile 把 data 写进同目录的临时文件，再改名替换 path。
func WriteFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
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
	if err := Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Rename 改名，Windows 上遇瞬时占用会重试。
//
// 已经把临时文件写好的地方（临时名由调用方决定）用它替换目标文件。
func Rename(oldpath, newpath string) error {
	err := os.Rename(oldpath, newpath)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	// 最长等约 75ms：够扫完一个几 KB 的 JSON，又不至于把一次保存拖成秒级。
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
