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
