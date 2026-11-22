package bundles

import (
	"context"
	"errors"
	"sort"
	"time"

	"wen/internal/i18n"
)

const (
	checkStartupGrace = 2 * time.Minute
	checkRetryAfter   = time.Hour
	perSourceTimeout  = 30 * time.Second
)

func (p *Plugin) loop(ctx context.Context) {
	defer p.wg.Done()
	for {
		next, ok := p.NextBackgroundAt(p.now())
		if !ok {
			// 配置或来源改变会重启插件；没有来源时用一个长等待让 Stop 仍能立即返回。
			next = p.now().Add(time.Hour)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-p.wake:
			timer.Stop()
		case <-timer.C:
			if err := p.RunBackground(ctx, p.now()); err != nil && ctx.Err() == nil {
				i18n.Logf(p.langOf(), "log.bundles.check_failed", "bundles: 部分插件包更新检查失败: %v", err)
			}
		}
	}
}

func (p *Plugin) wakeChecker() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// NextBackgroundAt 只读状态，不联网。没有绑定来源或关闭自动检查时没有排期。
func (p *Plugin) NextBackgroundAt(now time.Time) (time.Time, bool) {
	p.mu.Lock()
	auto, interval, started, lastTry := p.autoCheck, p.interval, p.started, p.lastTry
	p.mu.Unlock()
	if !auto {
		return time.Time{}, false
	}
	sources, err := p.store.Sources()
	if err != nil || len(sources) == 0 {
		return time.Time{}, false
	}
	var next time.Time
	for name, src := range sources {
		if pending, _ := p.store.Pending(name); pending != nil {
			continue
		}
		t := src.LastCheck.Add(interval)
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	if next.IsZero() {
		return time.Time{}, false
	}
	if earliest := started.Add(checkStartupGrace); next.Before(earliest) {
		next = earliest
	}
	if !lastTry.IsZero() {
		if retry := lastTry.Add(checkRetryAfter); next.Before(retry) {
			next = retry
		}
	}
	return next, true
}

// RunBackground 同步检查所有到期的来源。每个来源单独限时，一个失败不挡住其余。
func (p *Plugin) RunBackground(ctx context.Context, now time.Time) error {
	p.mu.Lock()
	auto, interval, lastTry := p.autoCheck, p.interval, p.lastTry
	p.mu.Unlock()
	if !auto || (!lastTry.IsZero() && now.Before(lastTry.Add(checkRetryAfter))) {
		return nil
	}
	sources, err := p.store.Sources()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)

	var errs []error
	for _, name := range names {
		src := sources[name]
		if now.Before(src.LastCheck.Add(interval)) {
			continue
		}
		if pending, _ := p.store.Pending(name); pending != nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, perSourceTimeout)
		_, err := p.store.CheckUpdateAt(cctx, name, now)
		cancel()
		if err != nil {
			errs = append(errs, err)
		}
	}
	p.mu.Lock()
	if len(errs) > 0 {
		p.lastTry = now
	} else {
		p.lastTry = time.Time{}
	}
	p.mu.Unlock()
	return errors.Join(errs...)
}
