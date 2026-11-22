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
// 取不到锁而目录又写不进去时（从只读快照导出），按只读数据放行：更新操作同样改不动
// 它，不存在需要互斥的并发切换。
func LockUpdates(ctx context.Context, lang, baseDir string) (func(), error) {
	unlock, err := NewStore(lang, baseDir).acquireUpdateLock(ctx)
	if err == nil {
		return unlock, nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// 等锁超时：另一个更新操作确实在跑。
		return nil, err
	}
	if dirWritable(filepath.Join(baseDir, StoreDirName, updateDirName)) {
		return nil, err
	}
	return func() {}, nil
}

// dirWritable 探一下目录能不能写。只在取锁失败时调用：写不进去说明是只读快照，
// 而不是有别人持着锁。
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".probe-")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
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
