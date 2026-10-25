package bookshelf

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	xunicode "golang.org/x/text/encoding/unicode"

	"wen/internal/plugin"
)

// 书架就是一个目录：里面每个 .txt / .md 文件是一本书，文件名（去掉扩展名）就是书名。
// 只认顶层文件，不进子目录——书架不需要分类，多一层目录只多一种「放进去了却找不到」。

// maxBookBytes 是单本书的大小上限。一部长篇小说几 MB，三十二兆足够；再大的多半不是书。
const maxBookBytes = 32 << 20

// book 是书架上的一本书。进度按文件名记，改了文件名就是另一本书。
type book struct {
	File  string
	Title string
}

var errTooBig = errors.New("too big")

// listBooks 列出书架上的书，按书名排序。目录不存在时返回空清单而不是报错：
// 还没放书和书架空着是一回事。
func listBooks(dir string) ([]book, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []book
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".txt" && ext != ".md" {
			continue
		}
		out = append(out, book{File: name, Title: strings.TrimSuffix(name, filepath.Ext(name))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

// findBook 按书名找书：先认全名，再认唯一的部分匹配（不分大小写）。找不到时返回
// 空，多于一本时返回全部候选，由调用方报给模型。
func findBook(books []book, name string) (book, []book) {
	name = strings.TrimSpace(strings.Trim(strings.TrimSpace(name), "《》"))
	for _, b := range books {
		if b.Title == name || b.File == name {
			return b, nil
		}
	}
	low := strings.ToLower(name)
	var hits []book
	for _, b := range books {
		if low != "" && strings.Contains(strings.ToLower(b.Title), low) {
			hits = append(hits, b)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	return book{}, hits
}

// textCache 缓存解码好的正文，只留最近的两本：读书是一段一段地读，同一本书连着
// 读几段时不该每次都把几兆的文件重新读一遍、解一遍码。按修改时间与大小判断失效。
type textCache struct {
	mu      sync.Mutex
	entries map[string]cachedText
	order   []string
}

type cachedText struct {
	mod  time.Time
	size int64
	text string
}

func (c *textCache) load(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Size() > maxBookBytes {
		return "", errTooBig
	}
	c.mu.Lock()
	if e, ok := c.entries[path]; ok && e.mod.Equal(st.ModTime()) && e.size == st.Size() {
		c.mu.Unlock()
		return e.text, nil
	}
	c.mu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := decodeText(raw)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]cachedText{}
	}
	if _, ok := c.entries[path]; !ok {
		c.order = append(c.order, path)
	}
	c.entries[path] = cachedText{mod: st.ModTime(), size: st.Size(), text: text}
	for len(c.order) > 2 {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	return text, nil
}

// decodeText 把文件内容转成 UTF-8 并统一换行。中文 txt 很多是 GBK 存的，记事本另存的
// 「Unicode」是带 BOM 的 UTF-16——这两样不认的话，书架上一半的书会是乱码。
func decodeText(raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}), bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		dec := xunicode.UTF16(xunicode.LittleEndian, xunicode.ExpectBOM).NewDecoder()
		if out, err := dec.Bytes(raw); err == nil {
			raw = out
		}
	default:
		raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
		raw = plugin.NormalizeEncoding(raw)
	}
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// runeCount 是「这本书有多少字」，给设置页报个大概。
func runeCount(s string) int { return utf8.RuneCountInString(s) }

// cutPassage 从 off（字节偏移）起取一段，约 size 个字，返回这一段与下一段的起点。
//
// 断在哪儿比取多少更要紧：拦腰切断的一句话读起来是坏的。到了字数时优先退回到最近的
// 段落结尾，其次是句子结尾（连同后面的引号），两者都不在后半段时才硬切——一整段几千字
// 不分段的文本也得能读下去。空白不计字数。
func cutPassage(text string, off, size int) (string, int) {
	if off < 0 {
		off = 0
	}
	off = skipSpace(text, off)
	if off >= len(text) {
		return "", len(text)
	}
	half := size / 2
	n, lastPara, lastSent := 0, -1, -1
	var prev rune // 上一个非空白字符
	i := off
	for i < len(text) && n < size {
		r, w := utf8.DecodeRuneInString(text[i:])
		i += w
		switch {
		case r == '\n':
			// 换行只在像段落结尾时才算：前面是句末，或者后面是空行。英文 txt 常按七十来列
			// 硬折行，每个换行都当段落的话会断在半句上
			if n >= half && (isSentenceEnd(prev) || prev == '.' || isCloser(prev) || strings.HasPrefix(text[i:], "\n")) {
				lastPara = i
			}
		case isSentenceEnd(r) || r == '.' && endsSentence(text[i:]):
			j := i
			for j < len(text) {
				c, cw := utf8.DecodeRuneInString(text[j:])
				if !isCloser(c) {
					break
				}
				j += cw
			}
			if n >= half {
				lastSent = j
			}
			n++
		case !unicode.IsSpace(r):
			n++
		}
		if !unicode.IsSpace(r) {
			prev = r
		}
	}
	end := i
	if skipSpace(text, end) < len(text) {
		switch {
		case lastPara > 0:
			end = lastPara
		case lastSent > 0:
			end = lastSent
		}
	}
	return tidy(text[off:end]), skipSpace(text, end)
}

func skipSpace(text string, i int) int {
	for i < len(text) {
		r, w := utf8.DecodeRuneInString(text[i:])
		if !unicode.IsSpace(r) {
			break
		}
		i += w
	}
	return i
}

func isSentenceEnd(r rune) bool { return strings.ContainsRune("。！？!?…", r) }

// endsSentence 判断英文句点是不是句末：后面接空白、引号或到了结尾。「3.14」「e.g」里的
// 点后面紧跟字符，不算。「Mr. Smith」会被误判，代价只是断点早了一点。
func endsSentence(rest string) bool {
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsSpace(r) || isCloser(r)
}

func isCloser(r rune) bool { return strings.ContainsRune("」』”’\"')）】", r) }

// tidy 去掉行尾空白，连续的空行并成一行。
func tidy(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	out := lines[:0]
	blank := false
	for _, l := range lines {
		l = strings.TrimRightFunc(l, unicode.IsSpace)
		if l == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// percent 是读到全书的几成。按字节算，中英混排时略有偏差，注入时本来就写「约」。
func percent(off, total int) int {
	if total <= 0 {
		return 0
	}
	if off >= total {
		return 100
	}
	p := off * 100 / total
	if p == 100 {
		p = 99
	}
	return p
}
