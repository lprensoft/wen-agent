package weather

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"wen/internal/i18n"
)

// 观测缓存的持久化。
//
// 缓存本来只在内存里，重启就空：循环一起来就全量重抓，于是每次重启都白烧一次
// API 调用，重启频繁时刷新间隔形同虚设；更要紧的是重启到抓到之间取不到天气，
// TurnPrompt 什么都不注入，模型会有一段「不知道外面什么天气」的空窗。
//
// 存下来之后，刷新时机由上次观测的时刻推算（见 untilRefresh），过期判定本来就
// 按时间戳算，加载回来自动正确。地名解析结果一并存：它几乎不变，重启后没必要
// 再向地理编码接口要一次。

const stateFile = "state.json"

// persisted 是落盘格式，按城市名原文分条。
type persisted struct {
	Places  map[string]Place  `json:"places,omitempty"`
	Reports map[string]Report `json:"reports,omitempty"`
	// Days 是按日留档（地点 → 日期 → 概要）。日记在第二天才来问那一天的天气，
	// 只存最近一次观测的话，隔几天一起收束时就补不齐了（见 dayfacts.go）。
	Days map[string]map[string]DayInfo `json:"days,omitempty"`
}

func statePath(dir string) string { return filepath.Join(dir, stateFile) }

// loadInto 把上次的观测装回缓存。文件缺失或损坏时什么都不做——
// 天气缓存是可以重新取的，坏了就当没有，不值得让插件起不来。
func (p *Plugin) loadInto(dir string, wanted []string) {
	if dir == "" {
		return
	}
	raw, err := os.ReadFile(statePath(dir))
	if err != nil {
		return
	}
	var st persisted
	if json.Unmarshal(raw, &st) != nil {
		i18n.Logf(p.langOf(), "log.weather.cache_corrupt", "weather: 观测缓存已损坏，忽略并重新获取")
		return
	}

	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	for _, loc := range wanted {
		o := p.entryLocked(loc)
		if pl, ok := st.Places[loc]; ok && pl.Name != "" {
			o.place, o.placeOK = pl, true
		}
		// 观测时刻缺失的条目直接丢弃：没有时刻就没法判断新鲜与否，
		// 而把来历不明的天气当成此刻的，正是这份缓存最不该做的事
		if rep, ok := st.Reports[loc]; ok && !rep.Fetched.IsZero() {
			o.cur, o.curOK = rep, true
		}
		if days, ok := st.Days[loc]; ok && len(days) > 0 {
			o.days = days
		}
	}
}

// save 落盘当前缓存。失败只影响下次启动的连续性，不值得打断刷新。
func (p *Plugin) save(dir string) {
	if dir == "" {
		return
	}
	p.dataMu.RLock()
	st := persisted{Places: map[string]Place{}, Reports: map[string]Report{}, Days: map[string]map[string]DayInfo{}}
	for loc, o := range p.obs {
		if o.placeOK {
			st.Places[loc] = o.place
		}
		if o.curOK {
			st.Reports[loc] = o.cur
		}
		if len(o.days) > 0 {
			st.Days[loc] = o.days
		}
	}
	p.dataMu.RUnlock()

	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	if err := os.WriteFile(statePath(dir), raw, 0o644); err != nil {
		i18n.Logf(p.langOf(), "log.weather.cache_write_failed", "weather: 观测缓存写入失败: %v", err)
	}
}

// untilRefresh 算出距下一次该刷新还有多久。
//
// 重启后不立刻抢一次：缓存还新鲜时等到该刷新的时刻。有一处没有数据就立刻取——
// 那一处正处在空窗里。
func (p *Plugin) untilRefresh(locs []string, refresh time.Duration, now time.Time) time.Duration {
	p.dataMu.RLock()
	defer p.dataMu.RUnlock()

	wait := refresh
	for _, loc := range locs {
		o := p.obs[loc]
		if o == nil || !o.curOK {
			return 0
		}
		if d := refresh - now.Sub(o.cur.Fetched); d < wait {
			wait = d
		}
	}
	if wait < 0 {
		return 0
	}
	return wait
}
