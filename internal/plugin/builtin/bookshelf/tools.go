package bookshelf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

func errNotReady(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.bookshelf.not_ready", "书架尚未就绪"))
}

// shelfList 是报错时附上的书名清单，让模型下一次能叫对名字。书名给全的，不截断：
// 截断过的名字拿去调工具对不上，两本前缀相同的书截断后更是分不出来。列不下的指向
// list_books，不能只写个省略号——那样第九本书她永远不知道叫什么。
func shelfList(lang string, books []book) string {
	var titles []string
	for i, b := range books {
		if i == maxListed {
			break
		}
		titles = append(titles, prompt.Tf(lang, "prompt.bookshelf.render.title", "《%s》", b.Title))
	}
	s := strings.Join(titles, prompt.T(lang, "prompt.bookshelf.render.sep", "、"))
	if len(books) > maxListed {
		s += prompt.Tf(lang, "prompt.bookshelf.err.more", "等 %d 本（全部书名用 list_books 查）", len(books))
	}
	return s
}

// resolve 把模型给的书名对到书架上的一本书；不给书名时接着读手上那本。
func resolve(lang string, books []book, st *State, name string) (book, error) {
	if strings.TrimSpace(name) == "" {
		if st.Current == "" {
			return book{}, fmt.Errorf(prompt.T(lang, "prompt.bookshelf.err.which", "没说是哪本。书架上有：%s"), shelfList(lang, books))
		}
		for _, b := range books {
			if b.File == st.Current {
				return b, nil
			}
		}
		return book{}, fmt.Errorf(prompt.T(lang, "prompt.bookshelf.err.current_gone",
			"手上那本已经不在书架上了。书架上有：%s"), shelfList(lang, books))
	}
	b, cands := findBook(books, name)
	if b.File != "" {
		return b, nil
	}
	if len(cands) > 1 {
		return book{}, fmt.Errorf(prompt.T(lang, "prompt.bookshelf.err.ambiguous",
			"有好几本对得上：%s。说全书名。"), shelfList(lang, cands))
	}
	return book{}, fmt.Errorf(prompt.T(lang, "prompt.bookshelf.err.no_such",
		"书架上没有「%s」。书架上有：%s"), name, shelfList(lang, books))
}

// ---------- read_book ----------

type readTool struct{ p *Plugin }

func (t *readTool) Name() string { return "read_book" }

func (t *readTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.bookshelf.tool.read_book.desc", readDescZH)
}

const readDescZH = "从书签处接着读一段书，读完书签自动往后挪。不写书名就接着读手上那本。" +
	"只在没人在跟前的时候能翻开新的一段。"

func (t *readTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.bookshelf.tool.read_book.schema", readSchemaZH))
}

const readSchemaZH = `{
	"type": "object",
	"properties": {
		"book": {"type": "string", "description": "书名；换一本读时写，接着读手上那本时不写"},
		"from_start": {"type": "boolean", "description": "从头读起，重读一本读过的书时用"}
	}
}`

func (t *readTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Book      string `json:"book"`
		FromStart bool   `json:"from_start"`
	}
	lang := t.p.langOf()
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	// 门禁在代码里而不只是提示词里：对面有人时一大段正文塞进上下文，等于让人看着她读书
	if plugin.IsInteractive(ctx) {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.bookshelf.err.interactive",
			"对方就在跟前，现在不翻开新的一段；读过的部分照常可以聊。"))
	}
	store := t.p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", errNotReady(lang)
	}
	shelf, size := t.p.settings()
	books, err := listBooks(shelf)
	if err != nil {
		return "", err
	}
	if len(books) == 0 {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.bookshelf.err.empty", "书架上还没有书。"))
	}

	var out string
	err = store.Update(func(st *State) error {
		b, err := resolve(lang, books, st, a.Book)
		if err != nil {
			return err
		}
		text, err := t.p.cache.load(filepath.Join(shelf, b.File))
		if err != nil {
			return openErr(lang, b, err)
		}
		now := t.p.clock()
		pr := st.Books[b.File]
		if pr == nil {
			pr = &Progress{Started: now}
			st.Books[b.File] = pr
		}
		title := b.Title // 工具结果只出现一次，给全名：截断过的名字拿去调工具对不上
		if a.FromStart {
			pr.Offset, pr.Finished, pr.Started = 0, time.Time{}, now
		}
		if !pr.Finished.IsZero() {
			out = prompt.Tf(lang, "prompt.bookshelf.read.already_done",
				"《%s》已经读完了（%s）。想重读就带上 from_start。", title, agoText(lang, pr.Finished, now))
			return nil
		}
		if pr.Offset > len(text) {
			pr.Offset = len(text) // 文件被换成了更短的版本
		}
		passage, next := cutPassage(text, pr.Offset, size)
		st.Current = b.File
		pr.LastRead, pr.Length = now, len(text)
		if passage == "" {
			pr.Offset, pr.Finished = len(text), now
			out = prompt.Tf(lang, "prompt.bookshelf.read.at_end", "《%s》已经读到结尾了。", title)
			return nil
		}
		pr.Offset = next
		out = prompt.Tf(lang, "prompt.bookshelf.read.result", "《%s》，这一段读完读到约 %d%%：\n\n%s",
			title, percent(next, len(text)), passage)
		if next >= len(text) {
			pr.Finished = now
			out += prompt.T(lang, "prompt.bookshelf.read.finished", "\n\n（全书读完了。）")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

func openErr(lang string, b book, err error) error {
	title := b.Title
	if errors.Is(err, errTooBig) {
		return fmt.Errorf(prompt.T(lang, "prompt.bookshelf.err.too_big", "《%s》太大了，读不了。"), title)
	}
	return fmt.Errorf(prompt.T(lang, "prompt.bookshelf.err.open", "《%s》打不开：%v"), title, err)
}

// ---------- list_books ----------

// listTool 给出书架的完整目录。[在读的书] 只列得下八本，书再多的时候她得有办法知道
// 其余的叫什么；目录走工具结果，只在需要时出现一次，不必压进每轮的注入里。
type listTool struct{ p *Plugin }

func (t *listTool) Name() string { return "list_books" }

func (t *listTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.bookshelf.tool.list_books.desc", listDescZH)
}

const listDescZH = "列出书架上的书（完整书名）与你各读到了哪儿，没读过的排前面。[在读的书] 列不下时用它；" +
	"书多时分页，也可以按书名里的字筛。"

func (t *listTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.bookshelf.tool.list_books.schema", listSchemaZH))
}

const listSchemaZH = `{
	"type": "object",
	"properties": {
		"query": {"type": "string", "description": "只列书名里含这些字的书"},
		"page": {"type": "integer", "description": "第几页，从 1 开始；不写就是第 1 页"}
	}
}`

// catalogPage 是目录一页的条数：单次输出要有界，再多的书靠翻页与筛选找到。
const catalogPage = 100

func (t *listTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		Page  int    `json:"page"`
	}
	lang := t.p.langOf()
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", plugin.BadArgs(lang, err)
		}
	}
	store := t.p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", errNotReady(lang)
	}
	shelf, _ := t.p.settings()
	books, err := listBooks(shelf)
	if err != nil {
		return "", err
	}
	if len(books) == 0 {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.bookshelf.err.empty", "书架上还没有书。"))
	}
	st, err := store.Load()
	if err != nil {
		return "", err
	}
	ordered := shelfOrder(books, st)
	query := strings.ToLower(strings.TrimSpace(a.Query))
	if query != "" {
		var hits []book
		for _, bk := range ordered {
			if strings.Contains(strings.ToLower(bk.Title), query) {
				hits = append(hits, bk)
			}
		}
		if len(hits) == 0 {
			return prompt.Tf(lang, "prompt.bookshelf.list.no_match", "书架上的 %d 本书里，书名都不含「%s」。",
				len(books), a.Query), nil
		}
		ordered = hits
	}
	pages := (len(ordered) + catalogPage - 1) / catalogPage
	page := max(a.Page, 1)
	if page > pages {
		return prompt.Tf(lang, "prompt.bookshelf.list.no_page", "只有 %d 页。", pages), nil
	}
	start := (page - 1) * catalogPage
	end := min(start+catalogPage, len(ordered))

	var b strings.Builder
	switch {
	case query != "":
		b.WriteString(prompt.Tf(lang, "prompt.bookshelf.list.head_query", "书名含「%s」的有 %d 本：", a.Query, len(ordered)))
	default:
		b.WriteString(prompt.Tf(lang, "prompt.bookshelf.list.head", "书架上有 %d 本书：", len(books)))
	}
	if pages > 1 {
		b.WriteString(prompt.Tf(lang, "prompt.bookshelf.list.page", "（第 %d / %d 页）", page, pages))
	}
	for _, bk := range ordered[start:end] {
		state := prompt.T(lang, "prompt.bookshelf.list.unread", "没读过")
		if pr := st.Books[bk.File]; pr != nil {
			if pr.Finished.IsZero() {
				state = prompt.Tf(lang, "prompt.bookshelf.list.reading", "读到约 %d%%", percent(pr.Offset, pr.Length))
			} else {
				state = prompt.T(lang, "prompt.bookshelf.list.finished", "读完了")
			}
		}
		b.WriteString(prompt.Tf(lang, "prompt.bookshelf.list.item", "\n· 《%s》：%s", bk.Title, state))
	}
	if page < pages {
		b.WriteString(prompt.Tf(lang, "prompt.bookshelf.list.next", "\n……还有 %d 本，用 page=%d 看下一页。",
			len(ordered)-end, page+1))
	}
	return b.String(), nil
}

// ---------- note_book ----------

type noteTool struct{ p *Plugin }

func (t *noteTool) Name() string { return "note_book" }

func (t *noteTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.bookshelf.tool.note_book.desc", noteDescZH)
}

const noteDescZH = "给读过的书留一句印象。正文读过会忘，印象会一直留着。写你自己的感受，不抄原文；" +
	"每本书留最近 5 条。"

func (t *noteTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.bookshelf.tool.note_book.schema", noteSchemaZH))
}

const noteSchemaZH = `{
	"type": "object",
	"properties": {
		"book": {"type": "string", "description": "书名；写手上那本时不写"},
		"note": {"type": "string", "description": "一句印象（60 字内）"}
	},
	"required": ["note"]
}`

func (t *noteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Book string `json:"book"`
		Note string `json:"note"`
	}
	lang := t.p.langOf()
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	note := strings.Join(strings.Fields(a.Note), " ")
	if note == "" {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.bookshelf.err.note_empty", "印象不能为空。"))
	}
	// 超长报错让模型自己压，不悄悄截断：截掉一半的感受，意思可能就变了
	if n := utf8.RuneCountInString(note); n > maxNoteRunes {
		return "", fmt.Errorf(prompt.T(lang, "prompt.bookshelf.err.note_long",
			"印象太长了（%d 字），压到 %d 字以内。"), n, maxNoteRunes)
	}
	store := t.p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", errNotReady(lang)
	}
	shelf, _ := t.p.settings()
	books, err := listBooks(shelf)
	if err != nil {
		return "", err
	}
	if len(books) == 0 {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.bookshelf.err.empty", "书架上还没有书。"))
	}
	var out string
	err = store.Update(func(st *State) error {
		b, err := resolve(lang, books, st, a.Book)
		if err != nil {
			return err
		}
		pr := st.Books[b.File]
		if pr == nil || (pr.Offset == 0 && pr.Finished.IsZero()) {
			return fmt.Errorf(prompt.T(lang, "prompt.bookshelf.err.unread",
				"还没读过《%s》，读过了才有印象。"), b.Title)
		}
		pct := percent(pr.Offset, pr.Length)
		if !pr.Finished.IsZero() {
			pct = 100
		}
		addNote(pr, Note{Text: note, At: t.p.clock(), Percent: pct})
		out = prompt.T(lang, "prompt.bookshelf.note.done", "记下了。")
		return nil
	})
	if err != nil {
		return "", err
	}
	return out, nil
}
