package weather

import (
	"sort"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 按日留档，供别处回头问「那一天是什么天气」（见 plugin.DayReporter）。
//
// 数据是白来的：取现况的那次请求本来就带回昨天、今天、明天的概要（见 client.go），
// 这里只是把已经拿到手的东西按日期存下来，不多打一次接口。
//
// 之所以要存：问的人（按日收束的日记）在**第二天**才来问，而缓存里只有最近一次观测。
// 正常情况下那次观测的「昨天」恰好就是要问的那天，但隔了几天没聊、多天一起收束时就
// 补不齐了。留一小段日历，这种情况才有答案。
//
// 同一天会被记两次：先是作为「今天」（当天的预报值），第二天再作为「昨天」（实测值）
// 覆盖掉——后到的更准，一律覆盖。

// maxDayRecords 是每个地点保留的天数。日记只回看几天，多留也只是占地方。
const maxDayRecords = 40

// recordDays 把一次观测里的昨天与今天记进日历。调用方不得持有 dataMu。
func (p *Plugin) recordDays(loc string, rep Report) {
	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	o := p.entryLocked(loc)
	if o.days == nil {
		o.days = map[string]DayInfo{}
	}
	for _, d := range []DayInfo{rep.Today, rep.Yesterday} {
		if d.known() {
			o.days[d.Date] = d
		}
	}
	if len(o.days) <= maxDayRecords {
		return
	}
	dates := make([]string, 0, len(o.days))
	for date := range o.days {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	for _, date := range dates[:len(dates)-maxDayRecords] {
		delete(o.days, date)
	}
}

// dayInfo 取某个地点某一天的存档。
func (p *Plugin) dayInfo(loc, date string) (DayInfo, bool) {
	p.dataMu.RLock()
	defer p.dataMu.RUnlock()
	o := p.obs[loc]
	if o == nil {
		return DayInfo{}, false
	}
	d, ok := o.days[date]
	return d, ok && d.known()
}

// DayFacts 报一句那天的天气。
//
// 取的是**角色所在地**：写日记的是角色，日记开头那句天气说的是它当时在的地方。
// 没配城市、那天没有存档时返回 nil——补不上的那天宁可少一句，不编。
func (p *Plugin) DayFacts(date string) []string {
	s := p.snapshot()
	if s.personaLoc == "" {
		return nil
	}
	d, ok := p.dayInfo(s.personaLoc, date)
	if !ok {
		return nil
	}
	return []string{prompt.Tf(s.lang, "prompt.weather.day_fact", "天气：%s %.0f~%.0f℃",
		conditions.For(s.lang, d.Condition), d.MinC, d.MaxC)}
}

var _ plugin.DayReporter = (*Plugin)(nil)
