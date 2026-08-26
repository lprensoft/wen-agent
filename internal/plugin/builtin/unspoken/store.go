// Package unspoken 提供「心里话」的系统插件：角色没说出口的事——对对方的真实看法、憋着
// 的话、在等的事、决定暂时不提的事——记成一份有上限的清单，每轮注入上下文，说出口或
// 释怀了就放下。
//
// 它补的是潜台词的连续性：活人嘴上说没事，心里记着，这口气能记三天；模型没有地方放
// 这口气，于是下一轮就真的没事了。清单落盘在会话之外，天然跨会话、跨压缩接力；心情
// 回落后起因会丢，这里能留住「为什么不高兴」。
//
// 本插件依赖 roleplay：心里话是角色的，没有角色就没有归属对象。对方本人的事实不记在
// 这里——那由 roleplay 的「我的信息」与记忆负责，这里只记「我心里」。
package unspoken

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wen/internal/prompt"
)

// maxTextRunes 是单条的长度上限。清单随每轮对话注入，单条必须有确定上界。超长时报错
// 让模型压缩后重试，而不是悄悄截断——被截掉的半句模型不知道丢了。
const maxTextRunes = 80

// Entry 是一条心里话。
type Entry struct {
	Text    string    `json:"text"`
	Created time.Time `json:"created"`
}

// KeepResult 是一次记录的回执素材。
type KeepResult struct {
	Index     int      // 这条在清单里的序号（从 1 起）
	Duplicate bool     // 已经记着同样的话，没有新增
	Dropped   []string // 为腾位置淘汰掉的最早几条
}

// Store 管理一个可见域的清单。单个 JSON 文件，每次操作重新读取不做缓存。
type Store struct {
	mu   sync.Mutex
	lang string
	dir  string
	path string
}

// NewStore 建立指向 <dir>/unspoken.json 的清单库。lang 决定报错的语言。
func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, dir: dir, path: filepath.Join(dir, "unspoken.json")}
}

// List 返回全部条目，按记下的先后。文件不存在时返回空列表。
func (s *Store) List() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Keep 记一条。清单满了淘汰最早的（上限被调小过时可能不止一条），腾出一个位置。
// 同样的话已经记着时不重复记。
func (s *Store) Keep(text string, now time.Time, maxEntries int) (KeepResult, error) {
	text = normalize(text)
	if text == "" {
		return KeepResult{}, fmt.Errorf("%s", prompt.T(s.lang, "prompt.unspoken.err.empty", "text 不能为空"))
	}
	if n := len([]rune(text)); n > maxTextRunes {
		return KeepResult{}, fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.too_long", "text 过长（%d 字，上限 %d 字），请压缩后重试"), n, maxTextRunes)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	es, err := s.load()
	if err != nil {
		return KeepResult{}, err
	}
	if i := indexOfExact(es, text); i >= 0 {
		return KeepResult{Index: i + 1, Duplicate: true}, nil
	}
	var res KeepResult
	if maxEntries > 0 {
		for len(es) >= maxEntries {
			res.Dropped = append(res.Dropped, es[0].Text)
			es = es[1:]
		}
	}
	es = append(es, Entry{Text: text, Created: now})
	if err := s.save(es); err != nil {
		return KeepResult{}, err
	}
	res.Index = len(es)
	return res, nil
}

// LetGo 放下一条。index 大于 0 时按序号（从 1 起）定位，否则按原文片段（大小写不敏感的
// 包含匹配）定位；片段命中多条时报错并列出候选，让模型说得更具体。
func (s *Store) LetGo(index int, fragment string) (Entry, error) {
	fragment = strings.ToLower(normalize(fragment))
	if index <= 0 && fragment == "" {
		return Entry{}, fmt.Errorf("%s", prompt.T(s.lang, "prompt.unspoken.err.need_locator", "要传 index（序号）或 text（原文片段）之一来指明放下哪一条"))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	es, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	if len(es) == 0 {
		return Entry{}, fmt.Errorf("%s", prompt.T(s.lang, "prompt.unspoken.err.empty_list", "心里没有记着的话，没什么可放下的"))
	}

	at := -1
	if index > 0 {
		if index > len(es) {
			return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.no_such_index", "没有第 %d 条，现在共 %d 条：\n%s"), index, len(es), listText(es))
		}
		at = index - 1
	} else {
		var hits []int
		for i, e := range es {
			if strings.Contains(strings.ToLower(e.Text), fragment) {
				hits = append(hits, i)
			}
		}
		switch len(hits) {
		case 0:
			return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.no_match", "没有哪一条含 %q，现有：\n%s"), fragment, listText(es))
		case 1:
			at = hits[0]
		default:
			// 候选保留原序号，模型据此直接传 index
			var b strings.Builder
			for _, i := range hits {
				fmt.Fprintf(&b, "%d. %s\n", i+1, es[i].Text)
			}
			return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.ambiguous", "%q 命中了 %d 条，请传序号或更具体的片段：\n%s"), fragment, len(hits), strings.TrimRight(b.String(), "\n"))
		}
	}

	removed := es[at]
	es = append(es[:at], es[at+1:]...)
	if len(es) == 0 {
		if err := s.remove(); err != nil {
			return Entry{}, err
		}
		return removed, nil
	}
	return removed, s.save(es)
}

// Clear 抹掉本库的清单。文件不存在时不算错误。
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remove()
}

func (s *Store) remove() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.clear", "清除心里话失败: %w"), err)
	}
	return nil
}

func (s *Store) load() ([]Entry, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.read", "读取心里话失败: %w"), err)
	}
	var es []Entry
	if err := json.Unmarshal(raw, &es); err != nil {
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.corrupt", "心里话文件损坏: %w"), err)
	}
	// 手改过的文件不该把长度上限撑破
	out := es[:0]
	for _, e := range es {
		e.Text = normalize(e.Text)
		if e.Text == "" {
			continue
		}
		if r := []rune(e.Text); len(r) > maxTextRunes {
			e.Text = string(r[:maxTextRunes])
		}
		out = append(out, e)
	}
	return out, nil
}

// save 原子写回，权限 0600：心里话属于对话内容的一部分。
func (s *Store) save(es []Entry) error {
	if es == nil {
		es = []Entry{}
	}
	raw, err := json.MarshalIndent(es, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.save", "保存心里话失败: %w"), err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.mkdir", "创建心里话目录失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.save", "保存心里话失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.unspoken.err.save", "保存心里话失败: %w"), err)
	}
	return nil
}

// normalize 压掉换行与连续空白。
func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func indexOfExact(es []Entry, text string) int {
	for i, e := range es {
		if strings.EqualFold(e.Text, text) {
			return i
		}
	}
	return -1
}

// listText 把条目列成带序号的几行，用在报错里给模型指路。
func listText(es []Entry) string {
	var b strings.Builder
	for i, e := range es {
		fmt.Fprintf(&b, "%d. %s\n", i+1, e.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}
