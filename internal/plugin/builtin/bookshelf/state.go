package bookshelf

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stateMaxRows 是状态页上最多列几本有进度的书。进度库本身很小，这里只是兜长度。
const stateMaxRows = 30

// State 把各可见域的读书进度交给设置页的「角色状态」。
//
// 书架上的全部书目仍由插件操作「看看书架」负责；这里只列读过的，按域分块，
// 其余人格域由界面折叠。进度属于人格，里人格读过的书表人格不知道。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	p.mu.RLock()
	base, shelf := p.base, p.shelf
	p.mu.RUnlock()
	if base == "" {
		return nil
	}

	books, _ := listBooks(shelf)
	titles := make(map[string]string, len(books))
	for _, b := range books {
		titles[b.File] = b.Title
	}

	var out []plugin.StateSection
	failed := false
	for _, tag := range plugin.ReadDomains(base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		st, err := store.Load()
		if err != nil {
			i18n.Logf(p.langOf(), "log.bookshelf.state_failed", "读取读书进度失败（域 %q）: %v", tag, err)
			failed = true
			continue
		}
		if len(st.Books) == 0 {
			continue
		}

		type entry struct {
			file string
			pr   *Progress
			at   time.Time
		}
		entries := make([]entry, 0, len(st.Books))
		reading, finished := 0, 0
		var latest time.Time
		for file, pr := range st.Books {
			if pr == nil {
				continue
			}
			at := pr.LastRead
			if !pr.Finished.IsZero() {
				at = pr.Finished
				finished++
			} else {
				reading++
			}
			if at.After(latest) {
				latest = at
			}
			entries = append(entries, entry{file: file, pr: pr, at: at})
		}
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.After(entries[j].at) })

		rows := make([]plugin.StateRow, 0, min(len(entries), stateMaxRows))
		for _, e := range entries {
			if len(rows) >= stateMaxRows {
				continue
			}
			meta := i18n.Tf(q.Lang, "plugin.bookshelf.state.percent", "读到 %d%%", percent(e.pr.Offset, e.pr.Length))
			if !e.pr.Finished.IsZero() {
				meta = i18n.T(q.Lang, "plugin.bookshelf.state.finished", "读完")
			}
			title := titles[e.file]
			if title == "" {
				title = strings.TrimSuffix(e.file, filepath.Ext(e.file))
			}
			detail := ""
			if n := len(e.pr.Notes); n > 0 {
				detail = e.pr.Notes[n-1].Text
			}
			rows = append(rows, plugin.StateRow{
				Meta: meta, Value: title, Detail: detail,
				ID:    e.file,
				Edits: shelfEdits(q.Lang),
			})
		}
		out = append(out, plugin.StateSection{
			Key:       "shelf",
			Title:     i18n.T(q.Lang, "plugin.bookshelf.state.title", "读书"),
			Summary:   i18n.Tf(q.Lang, "plugin.bookshelf.state.summary", "在读 %d · 读完 %d", reading, finished),
			Rows:      rows,
			Domain:    tag,
			UpdatedAt: latest,
			More:      len(entries) - len(rows),
		})
	}
	if len(out) == 0 {
		if failed {
			return nil
		}
		empty := i18n.T(q.Lang, "plugin.bookshelf.state.empty", "还没有读过书架上的书。")
		if len(books) == 0 {
			empty = i18n.T(q.Lang, "plugin.bookshelf.state.shelf_empty",
				"书架还是空的。把 .txt 或 .md 文件放进书架目录就能读。")
		}
		return []plugin.StateSection{{
			Key:   "shelf",
			Title: i18n.T(q.Lang, "plugin.bookshelf.state.title", "读书"),
			Empty: empty,
		}}
	}
	return out
}

// 状态页上的操作标识。静态、不翻译。
const (
	stateSection = "shelf"
	stateOpReset = "reset"
)

// shelfEdits 是一本书上的操作：把进度与印象清零，回到没读过的状态。
func shelfEdits(lang string) []plugin.StateEdit {
	return []plugin.StateEdit{{
		Op:      stateOpReset,
		Label:   i18n.T(lang, "plugin.bookshelf.state.reset", "重置进度"),
		Confirm: i18n.T(lang, "plugin.bookshelf.state.reset_confirm", "把这本书的进度与印象清零？之后会从开头读。"),
		Danger:  true,
	}}
}
