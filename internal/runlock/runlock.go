// Package runlock 管理「本配置目录下有一个服务正在运行」这件事。
//
// 它用两个文件，分工不同：
//
//   - wen.runlock 是**运行权**。进程启动时在上面取一把独占的文件锁，持有到退出，
//     于是同一配置目录只可能有一个服务实例。锁由操作系统持有，进程崩溃也会释放；
//     文件本身永不删除——删掉它会让「检查是否存在」与「取得锁」之间出现一段空窗。
//   - wen.lock 是**登记信息**（监听地址、PID、版本），供配置工具判断该走在线还是
//     离线模式：服务在跑时必须经它的 HTTP 接口改配置（改动立即生效，也不会被服务端
//     的全量状态重写抹掉），服务没跑时才直接改状态文件。它只是个提示，不是锁：
//     真正的存活判定由调用方去连那个地址，陈旧的文件（进程被 kill -9 掉）因此不会
//     造成误判，只是让调用方多探一次。
//
// 运行权按配置目录而不是按端口：端口只能挡住同端口的第二个实例，换了端口的那个仍会
// 写同一份状态文件，还会在启动时替换第一个实例正在读取的插件目录。
package runlock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const (
	fileName     = "wen.lock"
	lockFileName = "wen.runlock"
)

// Info 是运行中实例登记的信息。
type Info struct {
	PID     int    `json:"pid"`
	Addr    string `json:"addr"` // 实际监听地址 host:port（可能与配置文件不同）
	Version string `json:"version"`
	Started string `json:"started"`
}

// Path 返回登记文件的路径。
func Path(dir string) string {
	return filepath.Join(dir, fileName)
}

// ErrBusy 表示配置目录已被另一个存活的服务实例占用。
var ErrBusy = errors.New("同一配置目录已有实例在运行")

// Acquire 取得该配置目录的运行权并写入登记信息，返回释放函数。
//
// 已有存活实例时返回 ErrBusy，调用方应当退出：两个实例会互相覆盖状态文件，后来的
// 那个还会在启动时替换前者正在读取的插件目录。其它错误（目录不可写等）意味着没能
// 登记，配置工具会退回离线模式，服务本身仍可照常运行。
//
// 返回的释放函数可以重复调用。
func Acquire(dir string, info Info) (release func(), err error) {
	raw, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return func() {}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}, err
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if lockOpenBusy(err) {
			return func() {}, ErrBusy
		}
		return func() {}, err
	}
	unlock, err := lockExclusive(f)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, errLockBusy) {
			return func() {}, ErrBusy
		}
		return func() {}, err
	}
	if err := os.WriteFile(Path(dir), raw, 0o644); err != nil {
		_ = unlock()
		_ = f.Close()
		return func() {}, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// 先删登记文件、再放开运行权：反过来的话，新实例可能已经取得运行权并
			// 写下自己的登记，这里再删就把它的删掉了。
			_ = os.Remove(Path(dir))
			_ = unlock()
			_ = f.Close()
		})
	}, nil
}

// Read 读取登记文件。第二个返回值为 false 表示没有登记（文件缺失或内容损坏）。
// 文件存在不代表进程还活着，调用方需要自行探活。
func Read(dir string) (Info, bool) {
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		return Info{}, false
	}
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil || info.Addr == "" {
		return Info{}, false
	}
	return info, true
}
