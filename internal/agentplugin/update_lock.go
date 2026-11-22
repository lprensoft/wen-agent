package agentplugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"wen/internal/i18n"
)

const (
	updateLockFile = ".op.lock"
	updateLockWait = 50 * time.Millisecond
)

var (
	errUpdateLockBusy         = errors.New("插件包更新文件锁正被占用")
	openUpdateLockFile        = os.OpenFile
	updateLockOpenBusyTimeout = 5 * time.Minute
)

// LockUpdates 取得插件包更新目录的跨进程锁，返回释放函数。
//
// 备份这类要整体读取 bundles 目录的操作必须从头持有它：启动时的目录切换（先把旧包
// 移进 work，再把新包顶上）如果正好发生在预检与遍历之间，归档会漏掉那个包，却仍然
// 报告成功。
//
// 这是给**只读遍历**用的入口：更新目录里还没有锁文件时直接放行，也不去创建它。锁
// 文件由第一次更新操作建立并长期保留，没有它就说明这个目录里从来没有暂存或应用过
// 更新，也就不可能有正在进行的目录切换。只读快照（挂载的副本、从备份恢复的目录）
// 因此仍能导出，且不必往数据源里写任何东西。
//
// 判断依据是目录里有没有锁文件，而不是「我们能不能写」：备份账号可能读得到插件包、
// 却打不开服务账号建的 0600 锁文件，那种情况必须把错误报上去，不能当成只读目录。
func LockUpdates(ctx context.Context, lang, baseDir string) (func(), error) {
	lockPath := filepath.Join(baseDir, StoreDirName, updateDirName, updateLockFile)
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return func() {}, nil
	}
	return NewStore(lang, baseDir).acquireUpdateLock(ctx)
}

// acquireUpdateLock 跨进程串行化「读状态—修改—写回」。opMu 只管一个 Store
// 实例；两个 CLI 或 CLI 与服务同时写 sources.json 时，只有文件锁能防止丢更新。
// 锁由操作系统持有，进程退出或崩溃会自然释放；锁文件本身长期保留，不能靠删文件解锁。
func (s *Store) acquireUpdateLock(ctx context.Context) (func(), error) {
	dir := s.updatesDir()
	if dir == "" {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.err.no_store", "没有可用的插件包目录"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, updateLockFile)

	var openBusySince time.Time
	for {
		f, err := openUpdateLockFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
		if err == nil {
			openBusySince = time.Time{}
			unlock, lockErr := tryUpdateFileLock(f)
			if lockErr == nil {
				return func() {
					_ = unlock()
					_ = f.Close()
				}, nil
			}
			_ = f.Close()
			if !errors.Is(lockErr, errUpdateLockBusy) {
				return nil, lockErr
			}
		} else if !updateLockOpenBusy(err) {
			return nil, err
		} else {
			// Windows 也会把持久的目录权限问题报成 AccessDenied。只有真正持锁
			// 才值得等；连续打开失败必须有上限，否则启动会永远卡在后台重试。
			if openBusySince.IsZero() {
				openBusySince = time.Now()
			}
			if time.Since(openBusySince) >= updateLockOpenBusyTimeout {
				return nil, err
			}
		}

		timer := time.NewTimer(updateLockWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.lock", "等待插件包更新文件锁失败：%w"), ctx.Err())
		case <-timer.C:
		}
	}
}
