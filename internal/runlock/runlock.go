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
	"time"
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

// Run 是一个进程对某个配置目录的运行权。
type Run struct {
	dir    string
	f      *os.File
	unlock func() error
	pid    int
	once   sync.Once
}

// Lock 取得配置目录的运行权。同一目录已有存活实例（握着这把锁）时返回 ErrBusy，
// 调用方应当退出：两个实例会互相覆盖状态文件，后来的那个还会在启动时替换前者正在
// 读取的插件目录。
//
// 取锁与写登记分开，是因为**旧版服务只写 wen.lock、不持这把锁**。升级之后从另一个
// 端口启动的新版能顺利拿到锁，若这时直接覆盖登记，旧实例就没人知道了。调用方要在
// Lock 之后先核对已有登记（地址能应答就别启动），确认之后再 Register。
func Lock(dir string) (*Run, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if lockOpenBusy(err) {
			return nil, ErrBusy
		}
		return nil, err
	}
	unlock, err := lockExclusive(f)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, errLockBusy) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &Run{dir: dir, f: f, unlock: unlock}, nil
}

// Register 写下登记信息，供配置工具判断该走在线还是离线模式。
// 写入失败不影响运行权本身：调用方仍握着锁，只是配置工具会退回离线模式。
func (r *Run) Register(info Info) error {
	raw, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(Path(r.dir), raw, 0o644); err != nil {
		return err
	}
	r.pid = info.PID
	return nil
}

// Release 放开运行权。可以重复调用。
//
// 只删自己写下的那份登记：拒绝启动时还没有注册过，这时删文件会把仍在运行的旧实例
// 的登记一起抹掉，下一次启动就查不到它了。同样，期间若有旧版实例把自己的登记写进
// 来，也不能删。
func (r *Run) Release() {
	r.once.Do(func() {
		// 先删登记、再放开运行权：反过来的话，新实例可能已经取得运行权并写下自己
		// 的登记，这里再删就把它的删掉了。
		if r.pid != 0 {
			dropRegistration(Path(r.dir), r.pid)
		}
		_ = r.unlock()
		_ = r.f.Close()
	})
}

// dropRegistration 只删属于 pid 的那一份登记。
//
// 「读内容、再删文件」不是原子的：旧版实例不受运行权约束，它可能正好在这两步之间
// 完成登记，于是删掉的是它刚写下的地址。这里先把文件整体改名拿走，再看手里这份是
// 谁的——原路径在改名之后空出来，谁在这时写进去都不归我们删。若拿到的不是自己的
// 登记，原样放回。
func dropRegistration(path string, pid int) {
	trash := releasingPath(path)
	if err := os.Rename(path, trash); err != nil {
		return
	}
	if info, ok := readInfo(trash); ok && info.PID == pid {
		_ = os.Remove(trash)
		return
	}
	_ = os.Rename(trash, path)
}

// State 是一个配置目录里服务实例的判定结果。
type State int

const (
	// StateNone 表示确认没有实例在这个目录上运行。
	StateNone State = iota
	// StateRunning 表示登记指向的就是这个目录的实例。
	StateRunning
	// StateUnknown 表示有登记，但判不出它是否还对应这个目录（进程查不到身份、
	// 平台拿不到创建时间）。这种状态下不该写状态文件——一个仍在运行的服务下次
	// 全量重写会把改动抹掉；也不该把请求发给那个地址，它可能是别人的实例。
	StateUnknown
)

// Read 读取登记文件。第二个返回值为 false 表示没有可以使用的实例登记。
// 文件存在不代表进程还活着，调用方需要自行探活。
//
// 判不出身份（StateUnknown）时仍然返回 true：登记确实存在，只是没法核实。要写状态
// 文件的调用方必须改用 ReadState 并把 StateUnknown 当成「不能写」——这里给的
// 「有实例」正是为了不让它们退回离线写。
func Read(dir string) (Info, bool) {
	info, state := ReadState(dir)
	if state == StateNone {
		return Info{}, false
	}
	return info, true
}

// ReadState 与 Read 相同，但把「确认没有实例」与「判不出来」分开，供需要在两者
// 之间做不同决定的调用方使用（配置工具在后者必须停下）。
//
// 登记文件本身（wen.lock）由运行中的实例自己维护，读到就以它为准；只有接手中断
// 留下的备用登记（.releasing）才需要核实身份：旧实例退出时只清 wen.lock，这个文件
// 会留下，端口后来可能被另一份配置目录的服务复用。
//
// 核实分两步：进程还在（活着，或存在但无权查询），并且它的创建时间不晚于登记时间
// ——登记由进程自己在启动时写下，创建只可能更早；晚于它的是后来占了同一个 PID 的
// 新进程。
func ReadState(dir string) (Info, State) {
	if info, ok := readInfo(Path(dir)); ok {
		return info, StateRunning
	}
	info, ok := readInfo(releasingPath(Path(dir)))
	if !ok {
		return Info{}, StateNone
	}
	if processAlive(info.PID) == processGone {
		return Info{}, StateNone
	}
	started, err := time.Parse(time.RFC3339, info.Started)
	if err != nil {
		return info, StateUnknown
	}
	created, known := processStart(info.PID)
	if !known || created.After(time.Now().Add(registrationSkew)) {
		// 查不到创建时间，或查到的时间在未来（取错字段、换算不对）。
		return info, StateUnknown
	}
	if created.After(started.Add(registrationSkew)) {
		return Info{}, StateNone
	}
	return info, StateRunning
}

// processGone/processHere/processUnverifiable 是「进程在不在」的三种答案。
// 判不出来与确认不在必须分开：前者退回离线写文件，会踩到仍在运行的服务。
type liveness int

const (
	processGone liveness = iota
	processHere
	processUnverifiable
)

// registrationSkew 是「进程创建」与「登记时间」之间允许的倒挂。两者都取自同一台
// 机器的同一个时钟，正常只会差进程初始化那一下；留几秒是给 NTP 微调与平台取整。
const registrationSkew = 2 * time.Second

// releasingPath 是交接中被临时改名的登记文件。
func releasingPath(path string) string {
	return path + ".releasing"
}

func readInfo(path string) (Info, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Info{}, false
	}
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil || info.Addr == "" {
		return Info{}, false
	}
	return info, true
}
