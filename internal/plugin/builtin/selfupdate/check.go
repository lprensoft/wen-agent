package selfupdate

import (
	"context"
	"time"

	"wen/internal/i18n"

	"wen/internal/updater"
	"wen/internal/version"
)

// checkTimeout 是一次后台检查的时限。它是个小请求，不该长时间挂在那里。
const checkTimeout = 30 * time.Second

// retryAfter 是检查失败后的重试间隔。
//
// 它不能省：失败的那一次不会更新「上次检查的时刻」（那个时刻的含义是「查到过什么」），
// 于是下一轮算出来的时刻仍在过去，循环会立刻再试一遍——断网时就变成一刻不停地重连。
// 失败只记在内存里，重启后由启动宽限期兜住。
const retryAfter = time.Hour

// loop 是后台唯一的常驻 goroutine：到点查一次 GitHub 上最新的正式版。
//
// 它只查，不下载也不安装：无人值守地把用户脚下的程序换掉，不是一个该由定时器
// 做的决定。查到的结果落在设置页的按钮文案与状态行上。
func (p *Plugin) loop(ctx context.Context) {
	defer p.wg.Done()
	// 启动后先让这一阵子过去：服务刚起来时还有一堆插件在初始化
	earliest := p.now().Add(startupGrace)

	for {
		next := p.nextCheck(earliest)
		timer := time.NewTimer(time.Until(next)) // 已过期时零延时触发
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			p.backgroundCheck(ctx)
		}
	}
}

// nextCheck 推算下一次检查的时刻：上次检查 + 周期，失败过就再往后压一个重试间隔，
// 且一律不早于启动宽限期。
func (p *Plugin) nextCheck(earliest time.Time) time.Time {
	p.mu.RLock()
	next := p.st.LastCheck.Add(p.interval)
	if !p.lastTry.IsZero() {
		if t := p.lastTry.Add(retryAfter); t.After(next) {
			next = t
		}
	}
	p.mu.RUnlock()

	if next.Before(earliest) {
		return earliest
	}
	return next
}

// backgroundCheck 查一次，把结果记下来。失败只记日志——检查不到新版本不是故障，
// 断网、限流都会走到这里。
func (p *Plugin) backgroundCheck(ctx context.Context) {
	s := p.snapshot()
	p.mu.Lock()
	p.lastTry = p.now()
	p.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	rel, err := p.check(cctx, s)
	if err != nil {
		if ctx.Err() == nil { // 停止插件导致的取消不必报警
			i18n.Logf(p.langOf(), "log.selfupdate.check_failed", "self_update: 检查更新失败: %v", err)
		}
		return
	}
	if updater.Newer(version.Version, rel.Tag) {
		i18n.Logf(p.langOf(), "log.selfupdate.available", "self_update: 有新版 %s 可更新（当前 %s），在设置页的「程序更新」里一键更新",
			rel.Tag, version.Version)
	}
}
