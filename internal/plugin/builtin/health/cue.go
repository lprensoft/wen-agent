package health

// 延迟发作是这个插件里唯一需要代码「到点做事」的地方：模型在淋雨那一轮记下
// 「估计晚上发作」，到了晚上得有人提一句，否则下一次有人来聊才会发现角色病了。
// 本文件按分钟检查全部域里到点的状况，向 internal/cue 投一条开口理由，由心跳带进
// 主动开口的轮次。投过的标在落盘记录上（Cued），重启不重投；过了有效期还没投出去
// 的就此作罢——两小时后再说「刚开始不舒服」就是错的，宁可错过。

import (
	"context"
	"time"

	"wen/internal/cue"
	"wen/internal/prompt"
)

const (
	cueSource = "health"
	// cueTTL 是发作理由的有效期。过了这段时间状况本身已经由 TurnPrompt 接管。
	cueTTL = 2 * time.Hour
)

// cueKey 给一条状况的理由一个跨域唯一的键：同一个 id 在两个域里是两条状况。
func cueKey(tag, id string) string {
	if tag == "" {
		return id
	}
	return tag + "#" + id
}

// loop 是后台检查循环。
func (p *Plugin) loop(ctx context.Context, tick time.Duration) {
	defer p.wg.Done()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.postDueCues(time.Now())
		}
	}
}

// postDueCues 扫一遍全部域，为到点发作、还没投过理由的状况投递理由。
// 时刻由参数给出，测试不必等真实的定时器。
func (p *Plugin) postDueCues(now time.Time) {
	base, tags := p.allDomains()
	if base == "" {
		return
	}
	for _, tag := range tags {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		snap, err := store.Current(now)
		if err != nil {
			continue
		}
		for _, v := range snap.Active {
			c := v.Cond
			if c.Cued || now.Before(c.Onset) {
				continue
			}
			if now.Sub(c.Onset) < cueTTL {
				cue.Post(cue.Cue{
					Source: cueSource,
					Key:    cueKey(tag, c.ID),
					Text:   onsetText(p.langOf(), c),
					Expire: c.Onset.Add(cueTTL),
				})
			}
			_ = store.MarkCued(c.ID) // 投过或已错过都记下，下一拍不再看它
		}
	}
}

// onsetText 是发作理由的措辞。刚发作时总是最轻的一档，峰值更重时顺带说一句走向。
func onsetText(lang string, c Condition) string {
	start := band(lang, severityAt(c.Peak, 0.001))
	if peak := band(lang, c.Peak); peak != start {
		return prompt.Tf(lang, "prompt.health.cue.onset_rising",
			"先前记下的「%s」到点发作了——开始%s，接下来会往%s走。", c.Name, start, peak)
	}
	return prompt.Tf(lang, "prompt.health.cue.onset", "先前记下的「%s」到点发作了——开始%s。", c.Name, start)
}

// dropCue 撤回一条还没说出口的发作理由。痊愈与清除时用：已经送达的收不回，
// 还没送达的不该再送出去。
func dropCue(tag, id string) { cue.Drop(cueSource, cueKey(tag, id)) }
