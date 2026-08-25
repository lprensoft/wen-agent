package stylewatch

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"wen/internal/i18n"
	"wen/internal/stylecheck"
)

// 按天累计的统计。落盘是硬要求：「今日 42 轮」这种数字只在内存里攒的话，重启一次
// 就归零，而重启比一天更频繁是常态。

const (
	statsFile = "stats.json"
	// keepDays 是保留的天数。30 天够看出趋势，再长的跨度里提示词多半已经改过几版。
	keepDays = 30
)

// bucket 是一类轮次（真人 / 后台）在一天里的累计。
type bucket struct {
	Turns      int            `json:"turns"`
	HitTurns   int            `json:"hit_turns"` // 至少命中一处的轮数
	Hits       map[string]int `json:"hits,omitempty"`
	Runes      int            `json:"runes"`
	BracketSum float64        `json:"bracket_sum"` // 各轮【】占比之和，除以 Turns 得均值
	MaxRunes   int            `json:"max_runes"`
	MinRunes   int            `json:"min_runes"` // Turns 为 0 时无意义
}

func (b *bucket) add(m stylecheck.Metrics, hits []stylecheck.Hit) {
	if b.Turns == 0 || m.Runes < b.MinRunes {
		b.MinRunes = m.Runes
	}
	if m.Runes > b.MaxRunes {
		b.MaxRunes = m.Runes
	}
	b.Turns++
	b.Runes += m.Runes
	b.BracketSum += m.BracketRatio
	if len(hits) > 0 {
		b.HitTurns++
		if b.Hits == nil {
			b.Hits = map[string]int{}
		}
		for _, h := range hits {
			b.Hits[h.Rule]++
		}
	}
}

// totalHits 是命中的总处数。
func (b bucket) totalHits() int {
	n := 0
	for _, v := range b.Hits {
		n += v
	}
	return n
}

// avgRunes 是平均字数。
func (b bucket) avgRunes() int {
	if b.Turns == 0 {
		return 0
	}
	return b.Runes / b.Turns
}

// bracketPercent 是平均【】占比（百分数）。
func (b bucket) bracketPercent() int {
	if b.Turns == 0 {
		return 0
	}
	return int(b.BracketSum/float64(b.Turns)*100 + 0.5)
}

// hitBreakdown 把按规则的命中渲染成「客套收尾 2、加粗 1」，多的在前，最多 n 项。
func (b bucket) hitBreakdown(lang string, n int) string {
	return breakdown(lang, b.Hits, n)
}

func breakdown(lang string, hits map[string]int, n int) string {
	type kv struct {
		id string
		n  int
	}
	items := make([]kv, 0, len(hits))
	for id, c := range hits {
		items = append(items, kv{id, c})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].n != items[j].n {
			return items[i].n > items[j].n
		}
		return items[i].id < items[j].id
	})
	if n > 0 && len(items) > n {
		items = items[:n]
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s %d", stylecheck.LabelOf(lang, it.id), it.n))
	}
	return strings.Join(parts, i18n.T(lang, "plugin.style_watch.sep", "、"))
}

// summary 把一个 bucket 渲染成一句话：「42 轮，助手腔 3 次（客套收尾 2、加粗 1），均长 58 字，演绎占 23%」。
func (b bucket) summary(lang string) string {
	s := i18n.Tf(lang, "plugin.style_watch.summary.turns", "%d 轮，助手腔 %d 次", b.Turns, b.totalHits())
	if bd := b.hitBreakdown(lang, 3); bd != "" {
		s += i18n.Tf(lang, "plugin.style_watch.summary.breakdown", "（%s）", bd)
	}
	s += i18n.Tf(lang, "plugin.style_watch.summary.metrics", "，均长 %d 字，演绎占 %d%%", b.avgRunes(), b.bracketPercent())
	return s
}

// dayStat 是一天的统计，真人轮次与后台轮次分开计。
type dayStat struct {
	Date        string `json:"date"` // 本地日期 2006-01-02
	Interactive bucket `json:"interactive"`
	Background  bucket `json:"background"`
}

// stats 是整份统计，Days 按日期升序。
type stats struct {
	Days []dayStat `json:"days"`
}

// day 返回某天的统计（没有则新建），并把超出保留期的旧日子丢掉。
func (s *stats) day(date string) *dayStat {
	if i := s.index(date); i >= 0 {
		return &s.Days[i]
	}
	s.Days = append(s.Days, dayStat{Date: date})
	sort.Slice(s.Days, func(i, j int) bool { return s.Days[i].Date < s.Days[j].Date })
	if len(s.Days) > keepDays {
		s.Days = s.Days[len(s.Days)-keepDays:]
	}
	if i := s.index(date); i >= 0 {
		return &s.Days[i]
	}
	// 记的是比保留期还早的日子（时钟回拨），刚加进去就被裁掉了：给个不落盘的临时项
	return &dayStat{Date: date}
}

func (s stats) index(date string) int {
	for i := range s.Days {
		if s.Days[i].Date == date {
			return i
		}
	}
	return -1
}

// clone 深拷贝，供锁外写盘与渲染使用。
func (s stats) clone() stats {
	out := stats{Days: make([]dayStat, len(s.Days))}
	for i, d := range s.Days {
		d.Interactive.Hits = maps.Clone(d.Interactive.Hits)
		d.Background.Hits = maps.Clone(d.Background.Hits)
		out.Days[i] = d
	}
	return out
}

// find 返回某天的统计副本，没有时返回 false。
func (s stats) find(date string) (dayStat, bool) {
	if i := s.index(date); i >= 0 {
		return s.clone().Days[i], true
	}
	return dayStat{}, false
}

func statsPath(dir string) string { return filepath.Join(dir, statsFile) }

// loadStats 读回统计。文件缺失或损坏时从空开始——统计丢了只是少看一段趋势，
// 不值得让插件起不来。
func loadStats(dir string) stats {
	raw, err := os.ReadFile(statsPath(dir))
	if err != nil {
		return stats{}
	}
	var st stats
	if json.Unmarshal(raw, &st) != nil {
		return stats{}
	}
	sort.Slice(st.Days, func(i, j int) bool { return st.Days[i].Date < st.Days[j].Date })
	return st
}

func saveStats(dir string, st stats) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(statsPath(dir), raw, 0o600)
}
