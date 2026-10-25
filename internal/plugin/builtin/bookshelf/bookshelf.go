// Package bookshelf 提供读书的系统插件：对方把书（.txt / .md）放上书架，角色在没人
// 的时候自己一段一段地读，进度像书签一样落盘，读过的地方可以留一句印象。
//
// 它对抗的是模型的「全知」：一个人不会对什么书都了如指掌，她只知道读过的那些。正文
// 走工具结果进上下文、随压缩消失，留下来的只有她自己写的印象——读过不等于记得。读不读、
// 读哪本由模型定，插件只给书和进度。
//
// 只在没有真人在场的轮次里翻开新的一段（心跳、日程这类后台轮次）：聊着聊着一大段正文
// 塞进上下文，对面的人等于在看她读书。读过的部分随时能聊，印象每轮都在。
package bookshelf

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	defPassageRunes = 1200
	maxListed       = 8  // 书架清单最多列几本
	maxInProgress   = 3  // 「在读」最多列几本
	maxFinished     = 3  // 「读完了」最多列几本
	maxTitleRunes   = 40 // 书名注入时的上限：文件名是用户起的，可能很长
)

// Plugin 是 bookshelf 系统插件。
type Plugin struct {
	mu      sync.RWMutex
	lang    string
	shelf   string // 书架目录：放书的地方
	base    string // 进度库的基准目录（= 共享域）
	passage int    // 一段约多少字

	storesMu sync.Mutex
	stores   map[string]*Store

	cache textCache

	actMu    sync.Mutex
	actState plugin.ActionState

	clock func() time.Time
}

func New() *Plugin { return &Plugin{clock: time.Now} }

func (p *Plugin) Name() string { return "bookshelf" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "读书：对方放上书架的书，角色在没人的时候自己一段段地读，只知道读到的部分，读过留下印象"
}

// Requires 硬依赖 roleplay：读书的是角色，没有角色就没有那个「她」。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{{
		Key: "passage_runes", Label: "一段多少字", Type: plugin.FieldInt, Default: defPassageRunes,
		Min: plugin.IntPtr(300), Max: plugin.IntPtr(4000),
		Description: "每次读一段，约这么多字，断在段落或句子结尾。正文会进上下文，太长会让她更快犯困、更早触发压缩",
	}}
}

// Init 需要可写目录；顺手把书架目录建好，用户才找得到该往哪儿放书。可重入。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.bookshelf.no_state_dir",
			"没有可用的持久化目录，无法放置书架"))
	}
	shelf := filepath.Join(ictx.StateDir, "books")
	if err := os.MkdirAll(shelf, 0o700); err != nil {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.bookshelf.mkdir_failed", "创建书架目录失败: %w"), err)
	}

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.shelf = shelf
	p.base = filepath.Join(ictx.StateDir, "progress")
	p.passage = plugin.CfgInt(cfg, "passage_runes", defPassageRunes)
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&readTool{p: p}, &noteTool{p: p}}
}

// SystemPrompt 注入读书的判据，全部为静态内容。进度与印象随本轮可见域走 TurnPrompt。
func (p *Plugin) SystemPrompt() string { return guideFor(p.langOf()) }

func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

func (p *Plugin) settings() (shelf string, passage int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.shelf, p.passage
}

// storeFor 返回某个可见域的进度库（惰性创建）。未初始化时返回 nil。
func (p *Plugin) storeFor(tag string) *Store {
	p.mu.RLock()
	base, lang := p.base, p.lang
	p.mu.RUnlock()
	if base == "" {
		return nil
	}
	p.storesMu.Lock()
	defer p.storesMu.Unlock()
	if p.stores == nil {
		p.stores = map[string]*Store{}
	}
	if s, ok := p.stores[tag]; ok {
		return s
	}
	s := NewStore(lang, plugin.DomainDir(base, tag))
	p.stores[tag] = s
	return s
}

// TurnPrompt 注入书架、进度与印象。书架空着时不注入。
//
// 心跳的轮次里多一句「此刻没人在跟前，想读可以翻一段」：读书只在没人的时候，而心跳
// 正是那种时候——它在睡着、重忙时本来就不跳。这一句只说「可以」，读不读归模型。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	lang := p.langOf()
	store := p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", nil
	}
	shelf, _ := p.settings()
	books, err := listBooks(shelf)
	if err != nil || len(books) == 0 {
		return "", nil
	}
	st, err := store.Load()
	if err != nil {
		return "", err
	}
	idle := !plugin.IsInteractive(ctx) && plugin.TurnOriginFrom(ctx) == "heartbeat"
	body := render(lang, books, st, p.clock(), idle)
	return stateHeaderFor(lang) + "\n" + body, nil
}

// render 渲染读书状态。样子：
//
//	书架上有：《边城》、《呼兰河传》、《小王子》。
//	在读：《呼兰河传》读到约 23%，上次读是昨天；《小王子》读到约 40%，上次读是 12 天前。
//	读《呼兰河传》时你记下的：「祖父的园子写得真亮」（读到 10% 时）；「有二伯好可怜」（读到 21% 时）。
//	读完了：《边城》（30 天前）。
//	读《边城》时你记下的：「结尾那个人也许永远不回来了」（读到 100% 时）。
//	此刻没人在跟前，想读的话可以翻一段。
//
// 体量有界：书名、条数、印象都有上限。
func render(lang string, books []book, st State, now time.Time, idle bool) string {
	sep := prompt.T(lang, "prompt.bookshelf.render.sep", "、")
	part := prompt.T(lang, "prompt.bookshelf.render.part_sep", "；")
	titleOf := func(b book) string {
		return prompt.Tf(lang, "prompt.bookshelf.render.title", "《%s》", clipRunes(b.Title, maxTitleRunes))
	}

	var lines []string
	var titles []string
	for i, b := range books {
		if i == maxListed {
			break
		}
		titles = append(titles, titleOf(b))
	}
	if len(books) > maxListed {
		lines = append(lines, prompt.Tf(lang, "prompt.bookshelf.render.shelf_more", "书架上有 %d 本书：%s 等。",
			len(books), strings.Join(titles, sep)))
	} else {
		lines = append(lines, prompt.Tf(lang, "prompt.bookshelf.render.shelf", "书架上有：%s。", strings.Join(titles, sep)))
	}

	type entry struct {
		b  book
		pr *Progress
	}
	var reading, finished []entry
	readable := false
	for _, b := range books {
		pr := st.Books[b.File]
		switch {
		case pr == nil:
			readable = true
		case !pr.Finished.IsZero():
			finished = append(finished, entry{b, pr})
		default:
			readable = true
			reading = append(reading, entry{b, pr})
		}
	}
	sort.SliceStable(reading, func(i, j int) bool { return reading[i].pr.LastRead.After(reading[j].pr.LastRead) })
	sort.SliceStable(finished, func(i, j int) bool { return finished[i].pr.Finished.After(finished[j].pr.Finished) })
	if len(reading) > maxInProgress {
		reading = reading[:maxInProgress]
	}
	if len(finished) > maxFinished {
		finished = finished[:maxFinished]
	}

	notesLine := func(e entry, n int) {
		notes := e.pr.Notes
		if len(notes) > n {
			notes = notes[len(notes)-n:]
		}
		if len(notes) == 0 {
			return
		}
		var items []string
		for _, note := range notes {
			items = append(items, prompt.Tf(lang, "prompt.bookshelf.render.note", "「%s」（读到 %d%% 时）", note.Text, note.Percent))
		}
		lines = append(lines, prompt.Tf(lang, "prompt.bookshelf.render.notes", "读%s时你记下的：%s。",
			titleOf(e.b), strings.Join(items, part)))
	}

	if len(reading) > 0 {
		var items []string
		for _, e := range reading {
			items = append(items, prompt.Tf(lang, "prompt.bookshelf.render.reading_item", "%s读到约 %d%%，上次读是%s",
				titleOf(e.b), percent(e.pr.Offset, e.pr.Length), agoText(lang, e.pr.LastRead, now)))
		}
		lines = append(lines, prompt.Tf(lang, "prompt.bookshelf.render.reading", "在读：%s。", strings.Join(items, part)))
		// 最近读的那本多留几条印象，其余的各留最后一条：正读着的书心里装得多一些
		for i, e := range reading {
			if i == 0 {
				notesLine(e, 3)
			} else {
				notesLine(e, 1)
			}
		}
	}
	if len(finished) > 0 {
		var items []string
		for _, e := range finished {
			items = append(items, prompt.Tf(lang, "prompt.bookshelf.render.finished_item", "%s（%s）",
				titleOf(e.b), agoText(lang, e.pr.Finished, now)))
		}
		lines = append(lines, prompt.Tf(lang, "prompt.bookshelf.render.finished", "读完了：%s。", strings.Join(items, sep)))
		for _, e := range finished {
			notesLine(e, 1)
		}
	}
	if idle && readable {
		lines = append(lines, prompt.T(lang, "prompt.bookshelf.render.idle", "此刻没人在跟前，想读的话可以翻一段。"))
	}
	return strings.Join(lines, "\n")
}

// agoText 把时刻化成「今天 / 昨天 / N 天前」，按日历日算。零值或倒流时说「不久前」。
func agoText(lang string, t, now time.Time) string {
	day := func(x time.Time) time.Time {
		y, m, d := x.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, x.Location())
	}
	days := -1
	if !t.IsZero() {
		days = int(day(now).Sub(day(t.In(now.Location()))).Hours() / 24)
	}
	switch {
	case days < 0:
		return prompt.T(lang, "prompt.bookshelf.ago.unknown", "不久前")
	case days == 0:
		return prompt.T(lang, "prompt.bookshelf.ago.today", "今天")
	case days == 1:
		return prompt.T(lang, "prompt.bookshelf.ago.yesterday", "昨天")
	default:
		return prompt.Tf(lang, "prompt.bookshelf.ago.days", "%d 天前", days)
	}
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
