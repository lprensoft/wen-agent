package memory

import (
	"context"
	"time"

	"wen/internal/i18n"
)

// 本文件是记忆的逐步遗忘。只作用于保存时被标记为 Decay 的条目，未标记的记忆永久
// 保留，与本文件无关——一条记忆会不会失效是它自身的性质，不取决于产生它的场景。
//
// 两级：久未提及先把正文塌缩成摘要（细节没了、要点还在），更久之后移出记忆库。
// 两个时限都从 LastUsed 起算，常被提到的记忆因此永远走不到淡忘，这才对得上
// 「一直没有再提及」的语义。塌缩不调模型，理由见 Store.Blur。
//
// 上下文占用上有一点要说清楚：塌缩省的是 recall 时的 token 与模型的注意力，索引里
// 本来就只有那句摘要；真正让常驻索引变小的是移出记忆库这一级。

// runSweep 扫一遍全部可见域的记忆库，按时限淡忘与归档。在后台 goroutine 中执行，
// 由 OnTurnEnd 按天触发一次——遗忘是以天计的过程，不值得为它起定时器。
//
// 刻意不按可见域过滤：清扫不产生任何对模型可见的输出，维护动作本来就该覆盖全库。
func (p *Plugin) runSweep(s settings) {
	defer func() {
		p.turnMu.Lock()
		p.sweeping = false
		p.turnMu.Unlock()
	}()
	if !s.decay {
		return
	}

	now := time.Now()
	blurBefore := now.AddDate(0, 0, -s.blurDays)
	forgetBefore := now.AddDate(0, 0, -s.forgetDays)

	var blurred, forgotten int
	// 零值 Scope 让 ReadDomains 枚举出基准库与同级的全部可见域库
	for _, tag := range p.readDomains(context.Background()) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		entries, err := store.List()
		if err != nil {
			continue // 单个库读不出来不该让其余库也扫不成
		}
		for _, e := range entries {
			if !e.Decay {
				continue
			}
			switch {
			case e.LastUsed.Before(forgetBefore):
				if _, err := store.Archive(e.Name, now); err == nil {
					forgotten++
				}
			case !e.Blurred && e.LastUsed.Before(blurBefore):
				if _, err := store.Blur(e.Name); err == nil {
					blurred++
				}
			}
		}
	}

	if blurred == 0 && forgotten == 0 {
		return
	}
	// 只记条数不记标题：这行会进进程日志，而各可见域的记忆标题不该被写平到一处
	i18n.Logf(s.lang, "log.memory.decayed", "记忆淡忘：%d 条只剩要点，%d 条移出记忆库（可在记忆库的 %s 目录找回）",
		blurred, forgotten, forgottenDir)
}
