package phone

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	maxKept     = 30                  // 留下来可供细看的条数
	keepPerFeed = 5                   // 一次刷新每个源最多留几条新的：其余的算「划过去了」
	seenTTL     = 60 * 24 * time.Hour // 见过的条目记多久，过了就从去重表里删掉
	maxSeen     = 5000
)

// Item 是刷到的一条。Seen 是她刷到它的时刻，Hit 表示标题碰上了兴趣关键词。
type Item struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Title     string    `json:"title"`
	Link      string    `json:"link,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Published time.Time `json:"published,omitzero"`
	Seen      time.Time `json:"seen"`
	Hit       bool      `json:"hit,omitempty"`
}

// State 是小手机落盘的全部状态。
//
// LastRefresh 记「上一次刷的时刻」而不是「还剩多久」：重启之后照样从这个时刻推下一次，
// 不会一重启就刷一遍。Seen 是去重表，刷到过的条目不再当新的。
type State struct {
	LastRefresh time.Time            `json:"last_refresh,omitzero"`
	Seen        map[string]time.Time `json:"seen,omitempty"`
	Items       []Item               `json:"items,omitempty"`
	Errors      map[string]string    `json:"errors,omitempty"` // 每个源最近一次的错误，给设置页看
}

func statePath(dir string) string { return filepath.Join(dir, "phone.json") }

func loadState(dir string) (State, error) {
	raw, err := os.ReadFile(statePath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

// saveState 原子写回。
func saveState(dir string, st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := statePath(dir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// merge 把一个源这次取到的条目记进去重表，返回其中新留下来的几条。条目本身由调用方
// 在全部源都并完之后用 keep 一次收进缓存——逐源裁剪的话，后面的源会把前面的源刚刷到
// 的挤掉，而开口理由是从全部新条目里挑的，挑中的那条可能已经不在了。
//
// 每个源只留最新的 keepPerFeed 条，其余新条目只记进去重表：第一次启用、或者睡了一夜
// 醒来时，一个源可能一下子冒出二十条，人刷手机也不会一条条看完，往下划过去的就是
// 划过去了，下次也不会再当新的冒出来。
//
// 去重键带上订阅源地址：没有 guid 与链接的条目只能按标题认，两个源恰好发了同名的
// 消息时，不带源的话后一个会被当成见过而吞掉。
func merge(st *State, feedURL, source string, entries []entry, interests []string, now time.Time) []Item {
	if st.Seen == nil {
		st.Seen = map[string]time.Time{}
	}
	var fresh []entry
	for _, e := range entries {
		e.ID = hashID(feedURL + "\x00" + e.ID)
		if _, ok := st.Seen[e.ID]; ok {
			continue
		}
		st.Seen[e.ID] = now
		fresh = append(fresh, e)
	}
	// 新的排前面：发布时间晚的先；没有发布时间的保持源里的顺序（源本来就是新的在前）
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Published.After(fresh[j].Published) })
	if len(fresh) > keepPerFeed {
		fresh = fresh[:keepPerFeed]
	}
	var added []Item
	for _, e := range fresh {
		it := Item{
			ID: e.ID, Source: source, Title: e.Title, Link: e.Link, Summary: e.Summary,
			Published: e.Published, Seen: now, Hit: matches(e.Title, interests),
		}
		if it.Published.After(now) {
			it.Published = time.Time{} // 时钟不准的源：未来的发布时间不可信
		}
		added = append(added, it)
	}
	return added
}

// keep 把这次新刷到的收进缓存：新的在前（发布晚的先），pick 指定的那条（开口理由选中
// 的）排在最前，然后统一裁到 maxKept——理由说的那条新闻必须还能用 read_news 细看。
func keep(st *State, added []Item, pick string) {
	fresh := append([]Item(nil), added...)
	sort.SliceStable(fresh, func(i, j int) bool {
		if (fresh[i].ID == pick) != (fresh[j].ID == pick) {
			return fresh[i].ID == pick
		}
		return fresh[i].when().After(fresh[j].when())
	})
	st.Items = append(fresh, st.Items...)
	if len(st.Items) > maxKept {
		st.Items = st.Items[:maxKept]
	}
}

// prune 清掉去重表里太旧的记录，并把总数压在上限内。
func prune(st *State, now time.Time) {
	for id, at := range st.Seen {
		if now.Sub(at) > seenTTL {
			delete(st.Seen, id)
		}
	}
	if len(st.Seen) <= maxSeen {
		return
	}
	type kv struct {
		id string
		at time.Time
	}
	all := make([]kv, 0, len(st.Seen))
	for id, at := range st.Seen {
		all = append(all, kv{id, at})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, e := range all[:len(all)-maxSeen] {
		delete(st.Seen, e.id)
	}
}
