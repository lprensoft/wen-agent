package weather

// 天气是最自然的开口理由产地：「刚下起雨了」「明天有雨，记得带伞」比任何无话找话
// 都像真人。本文件在每次刷新后对比前后两次观测，两类事向 internal/cue 投递理由，
// 由心跳带进主动开口的轮次：现况里降水的开始或停止，以及明天的预报里出现降水。
//
// 只报「转变」，不报状态——此刻的天气与明天的概要本来就由 TurnPrompt 每轮注入，
// 重复报状态只是噪声；转变才是「值得现在说」的事。前一次观测太旧时不报现况转变：
// 宕机横跨的转变说「刚下起来」就是错的，宁可错过。

import (
	"strings"
	"time"

	"wen/internal/cue"
	"wen/internal/prompt"
)

// cueTTL 是天气理由的有效期。「刚下起雨」过了这个时长还没说出口就不再算数——
// 那时天气状态早由 TurnPrompt 注入的现况接管了。
const cueTTL = 90 * time.Minute

// maybePostCue 对比一处地点的前后两次观测，值得开口时投递理由。
// 现况转变以 (weather, 地点) 为键：同一地点更晚的转变覆盖更早的——先下起又停了时，
// 该说的是「停了」，而不是按顺序把两件事都播一遍。
func (p *Plugin) maybePostCue(loc string, prev Report, prevOK bool, cur Report) bool {
	s := p.snapshot()
	prefix := s.rolePrefix(s.lang, loc)
	if prefix == "" {
		return false // 配置已不再关心的地点
	}
	// 现况的转变：前一次观测断档太久（超过两个刷新周期）就当没有先前状态
	if prevOK && cur.Fetched.Sub(prev.Fetched) <= 2*s.refresh {
		if text := changeText(s.lang, prefix, prev, cur); text != "" {
			cue.Post(cue.Cue{Source: "weather", Key: loc, Text: text, Expire: cur.Fetched.Add(cueTTL)})
		}
	}
	return p.forecastCue(s.lang, prefix, loc, prev, cur, s.inTomorrowWindow(cur.Fetched))
}

// forecastCue 盯着明天的预报：预报里出现降水时投一条理由——「明天有雨，记得带伞」
// 是最经典的主动关心；降水又从预报里消失时撤回，还没说出口的不该再说。
//
// 只在「看明天」的时段里投（白天的人不会为明天的雨开口），且每一天只投一次
// （Cued 随观测延续，刷新不重投）：理由可能已被心跳说出口，重投会让它反复念叨。
// 理由的有效期到今天为止——过了午夜「明天有雨」这句话就指错了日子。
// 返回本次是否投递了，调用方据此把 Cued 记到缓存上。
func (p *Plugin) forecastCue(lang, prefix, loc string, prev, cur Report, inWindow bool) bool {
	tm := cur.Tomorrow
	if !tm.known() {
		return false
	}
	key := loc + "|" + tm.Date
	prevWetCued := prev.Tomorrow.known() && prev.Tomorrow.Date == tm.Date && isWet(prev.Tomorrow.Condition) && prev.Tomorrow.Cued
	switch {
	case isWet(tm.Condition):
		if tm.Cued || !inWindow {
			return false // 已投过，或还没到看明天的时候
		}
		expire, err := time.ParseInLocation("2006-01-02", tm.Date, time.Local)
		if err != nil {
			return false
		}
		text := prompt.Tf(lang, "prompt.weather.cue.forecast", "%s%s明天预报有%s（%.0f~%.0f℃）。",
			prefix, cur.Place, conditions.For(lang, tm.Condition), tm.MinC, tm.MaxC)
		cue.Post(cue.Cue{Source: "weather", Key: key, Text: text, Expire: expire})
		return true
	case prevWetCued:
		cue.Drop("weather", key) // 雨从预报里消失了，撤回还没说出口的那句
	}
	return false
}

// rolePrefix 给出「这处地点是谁的」的措辞前缀，与 TurnPrompt 的注入措辞一致。
func (s settings) rolePrefix(lang, loc string) string {
	switch {
	case s.sameCity && loc == s.personaLoc:
		return prompt.T(lang, "prompt.weather.prefix.both", "你与对方所在的")
	case loc == s.personaLoc:
		return prompt.T(lang, "prompt.weather.prefix.persona", "你所在的")
	case loc == s.userLoc:
		return prompt.T(lang, "prompt.weather.prefix.user", "对方所在的")
	}
	return ""
}

// changeText 把一次值得开口的天气转变写成一句话；无事可说返回空串。
func changeText(lang, prefix string, prev, cur Report) string {
	wasWet, nowWet := isWet(prev.Condition), isWet(cur.Condition)
	switch {
	case !wasWet && nowWet:
		return prompt.Tf(lang, "prompt.weather.cue.started", "%s%s刚下起了%s。",
			prefix, cur.Place, conditions.For(lang, cur.Condition))
	case wasWet && !nowWet:
		return prompt.Tf(lang, "prompt.weather.cue.stopped", "%s%s的%s停了，现在%s。",
			prefix, cur.Place, conditions.For(lang, prev.Condition), conditions.For(lang, cur.Condition))
	}
	return ""
}

// isWet 判断一个天气现象是否属于降水。按中文说法的用字判断而不是 WMO 代码：
// 观测缓存里的旧条目没有存代码，而这些说法是本插件自己的固定词表。
//
// 它认的是**规范值**，与界面语言无关——缓存里存的一律是中文，译名只出现在展示时。
func isWet(condition string) bool {
	for _, w := range []string{"雨", "雪", "雹"} {
		if strings.Contains(condition, w) {
			return true
		}
	}
	return false
}
