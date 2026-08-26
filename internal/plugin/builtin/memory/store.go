// Package memory 提供跨会话长期记忆的系统插件。
//
// 每条记忆是一个带 YAML frontmatter 的 Markdown 文件，可直接用编辑器查看与修改；
// 记忆索引不落盘，由文件头部信息实时生成，避免索引与正文各自漂移。
package memory

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"wen/internal/prompt"
)

const (
	defaultType = "事实"
	// typeBoundary 是唯一被代码按名字区分的分类：自动提炼路径强制它不淡忘。
	typeBoundary = "边界"
	// descMaxRunes 限制钩子长度：索引每一轮对话都会重复发送，单条必须有确定上界。
	descMaxRunes = 40
	// slugMaxRunes 远离各文件系统的长度边界（NTFS 上限为 255 个 UTF-16 单元）。
	slugMaxRunes = 60
	// forgottenDir 存放被自动遗忘的记忆，位于记忆库目录之下，不进索引。
	forgottenDir = "forgotten"
	// blurNoteZH 附在塌缩后的正文末尾，让读到它的一方知道这里本来还有细节。
	// 它会写进记忆文件，用的是塌缩发生时的语言——已落盘的内容不随语言改写。
	blurNoteZH = "（更早的细节已经记不清了。）"
)

// typeRank 返回分类的展示序，未知分类排在最后。
func typeRank(t string) int {
	if i := slices.Index(Types, t); i >= 0 {
		return i
	}
	return len(Types)
}

// Entry 是一条记忆。
type Entry struct {
	Slug        string // 文件名（不含扩展名）
	Name        string // 标题
	Description string // 一句话钩子，进索引
	Type        string
	Created     time.Time
	Updated     time.Time
	Content     string // 正文
	// LastUsed 是这条记忆最后一次被用到的时间（读取、修订、或在对话中被提及）。
	// 遗忘的两个时限都从它起算——「一直没有再提及」说的就是这个。
	// 旧文件没有该字段时读作 Updated，使升级前的记忆从最后一次改动起算。
	LastUsed time.Time
	// Decay 表示这条记忆会随时间淡忘。默认为 false（永久保留），
	// 由保存方按内容性质决定，与记忆分类正交。
	Decay bool
	// Blurred 表示正文已经塌缩成摘要，只剩要点。
	Blurred bool
	// Domain 是这条记忆所属的可见域标签，不落盘：它由所在的库决定，
	// 在跨库合并后用于把 recall / delete 送回正确的库。
	Domain string
}

// Store 管理记忆目录。目录内容可被用户在进程外直接修改，因此每次读取都会
// 按目录指纹校验缓存；自身的写入直接置脏，不依赖时间戳精度。
type Store struct {
	mu       sync.RWMutex
	lang     string
	dir      string
	cache    []Entry
	cacheKey uint64
	dirty    bool
}

// NewStore 建一个指向 dir 的记忆库。lang 决定报错与塌缩注记的语言。
func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, dir: dir, dirty: true}
}

func (s *Store) Dir() string { return s.dir }

// path 返回 slug 对应的文件路径，并确认它没有逃出记忆目录。
func (s *Store) path(slug string) (string, error) {
	p := filepath.Join(s.dir, slug+".md")
	if filepath.Dir(filepath.Clean(p)) != filepath.Clean(s.dir) {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.bad_slug", "非法的记忆名称 %q"), slug)
	}
	return p, nil
}

// List 返回全部记忆，按（分类, 标题）稳定排序。目录不存在时返回空列表。
func (s *Store) List() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *Store) listLocked() ([]Entry, error) {
	key, names, err := s.scan()
	if err != nil {
		return nil, err
	}
	if !s.dirty && key == s.cacheKey && s.cache != nil {
		return s.cache, nil
	}

	entries := make([]Entry, 0, len(names))
	for _, fname := range names {
		e, err := s.read(fname)
		if err != nil {
			continue // 单个文件损坏不影响其余记忆可用
		}
		entries = append(entries, e)
	}
	sortEntries(entries)

	s.cache, s.cacheKey, s.dirty = entries, key, false
	return entries, nil
}

// scan 列出记忆文件并计算目录指纹（文件名 + 大小 + 修改时间）。
func (s *Store) scan() (uint64, []string, error) {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, err
	}
	h := fnv.New64a()
	var names []string
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		names = append(names, name)
		fi, err := de.Info()
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s|%d|%d\n", name, fi.Size(), fi.ModTime().UnixNano())
	}
	return h.Sum64(), names, nil
}

// Get 按标题或文件名查找一条记忆（大小写不敏感）。
func (s *Store) Get(name string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.listLocked()
	if err != nil {
		return Entry{}, err
	}
	if e, ok := findEntry(entries, name); ok {
		return e, nil
	}
	return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.not_found", "没有名为 %q 的记忆"), name)
}

// isTypeName 判断一段文字是不是某种语言下的分类名。
func isTypeName(s string) bool {
	_, ok := memTypes.Canonical(strings.TrimSpace(s))
	return ok
}

// findEntry 依次尝试标题精确匹配与 slug 匹配，均不区分大小写。
// 索引里一条记忆显示为「分类/标题」，模型很自然会把整串当成标题传进来，故先剥掉分类前缀。
func findEntry(entries []Entry, name string) (Entry, bool) {
	name = strings.TrimSpace(name)
	// 前缀认任意语言的分类名：索引按界面语言渲染，模型照着它回传。
	if typ, rest, ok := strings.Cut(name, "/"); ok && isTypeName(typ) {
		name = strings.TrimSpace(rest)
	}
	want := strings.ToLower(name)
	if want == "" {
		return Entry{}, false
	}
	for _, e := range entries {
		if strings.ToLower(e.Name) == want {
			return e, true
		}
	}
	// 这里的报错被丢弃，语言无关紧要。
	slug, err := slugify("", name)
	if err != nil {
		return Entry{}, false
	}
	for _, e := range entries {
		if strings.EqualFold(e.Slug, slug) {
			return e, true
		}
	}
	return Entry{}, false
}

// Save 写入一条记忆。replace 为 false 时拒绝覆盖已存在的同名条目；
// 覆盖前会把原文件另存为 .bak。返回最终落盘的条目。
func (s *Store) Save(e Entry, replace bool) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e.Name = strings.TrimSpace(e.Name)
	if e.Name == "" {
		return Entry{}, fmt.Errorf("%s", prompt.T(s.lang, "prompt.memory.err.empty_title", "记忆标题不能为空"))
	}
	slug, err := slugify(s.lang, e.Name)
	if err != nil {
		return Entry{}, err
	}
	e.Slug = slug
	e.Description = truncateRunes(strings.TrimSpace(collapseSpace(e.Description)), descMaxRunes)
	e.Content = strings.TrimSpace(e.Content)
	if e.Type == "" {
		e.Type = defaultType
	}
	// 模型可能报回本语言的分类名，先换回中文规范值再落盘——磁盘上只有一种写法。
	zhType, ok := memTypes.Canonical(e.Type)
	if !ok {
		return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.unknown_type",
			"未知的记忆分类 %q（可选：%s）"), e.Type, strings.Join(memTypes.Values(s.lang), " / "))
	}
	e.Type = zhType

	entries, err := s.listLocked()
	if err != nil {
		return Entry{}, err
	}
	old, exists := findEntry(entries, e.Name)
	if exists && !replace {
		return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.exists", "已存在同名记忆 %q，如需覆盖请把 mode 设为 replace"), old.Name)
	}

	now := time.Now()
	e.Updated = now
	e.LastUsed = now  // 写入本身就是一次「用到」
	e.Blurred = false // 正文是刚写进来的，不再是塌缩后的残余
	if exists {
		e.Slug = old.Slug // 沿用原文件名，避免标题大小写变化产生第二个文件
		e.Created = old.Created
		// 淡忘标记只增不减。自动提炼在修订一条记忆时未必会重复带上这个标记，
		// 若按缺省值覆盖，一条本该淡忘的记忆会被一次无关的修订变成永久记忆。
		// 确实要取消淡忘，删掉重存或直接改文件头。
		e.Decay = e.Decay || old.Decay
	} else {
		e.Created = now
	}
	if e.Created.IsZero() {
		e.Created = now
	}

	p, err := s.path(e.Slug)
	if err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.mkdir", "创建记忆目录失败: %w"), err)
	}
	if exists {
		if raw, err := os.ReadFile(p); err == nil {
			_ = os.WriteFile(p+".bak", raw, 0o644)
		}
	}
	if err := writeFileAtomic(s.lang, p, render(e)); err != nil {
		return Entry{}, err
	}
	s.dirty = true
	return e, nil
}

// Delete 删除一条记忆。
func (s *Store) Delete(name string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.listLocked()
	if err != nil {
		return Entry{}, err
	}
	e, ok := findEntry(entries, name)
	if !ok {
		return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.not_found", "没有名为 %q 的记忆"), name)
	}
	p, err := s.path(e.Slug)
	if err != nil {
		return Entry{}, err
	}
	if err := os.Remove(p); err != nil {
		return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.delete", "删除记忆失败: %w"), err)
	}
	s.dirty = true
	return e, nil
}

// Touch 把一条记忆的最后使用时间刷成 now，不改动正文与更新时间。
// 已经是同一天的直接返回 false 不写盘：Store 的缓存靠目录指纹（含文件修改时间），
// 每次提及都改写会让整个记忆库的每一次读取都重新扫盘。
func (s *Store) Touch(name string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.listLocked()
	if err != nil {
		return false, err
	}
	e, ok := findEntry(entries, name)
	if !ok {
		return false, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.not_found", "没有名为 %q 的记忆"), name)
	}
	if sameDay(e.LastUsed, now) {
		return false, nil
	}
	e.LastUsed = now
	if err := s.writeLocked(e); err != nil {
		return false, err
	}
	return true, nil
}

// Blur 把正文塌缩成摘要，只留要点，并标记为已淡忘。
//
// 刻意不调模型：摘要是保存时就写好的一句话概括，直接拿来用是确定的、零成本的，
// 也更接近真实的遗忘——细节先掉、要点还在。也刻意不改 Updated 与 LastUsed：
// 淡忘是时间流逝的结果而不是一次使用，改了这条记忆就永远等不到归档。
func (s *Store) Blur(name string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.listLocked()
	if err != nil {
		return Entry{}, err
	}
	e, ok := findEntry(entries, name)
	if !ok {
		return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.not_found", "没有名为 %q 的记忆"), name)
	}
	if e.Blurred {
		return e, nil
	}
	gist := e.Description
	if gist == "" {
		gist = truncateRunes(firstLine(e.Content), descMaxRunes)
	}
	e.Content = strings.TrimSpace(gist + "\n\n" + blurNoteZH)
	e.Blurred = true
	if err := s.writeLocked(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Archive 把一条记忆移出记忆库，落到 forgotten/ 子目录。
//
// 不用 os.Remove：自动遗忘是这套机制里唯一不可逆、而且误删完全无从察觉的动作——
// 用户根本不会知道曾经有过这条。扫描只认本级目录下的 .md，子目录天然不进索引。
func (s *Store) Archive(name string, now time.Time) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.listLocked()
	if err != nil {
		return Entry{}, err
	}
	e, ok := findEntry(entries, name)
	if !ok {
		return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.not_found", "没有名为 %q 的记忆"), name)
	}
	src, err := s.path(e.Slug)
	if err != nil {
		return Entry{}, err
	}
	dir := filepath.Join(s.dir, forgottenDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.mkdir_forgotten", "创建遗忘目录失败: %w"), err)
	}
	// 文件名带归档日期：同一条记忆被遗忘后又重新记起、再被遗忘是正常路径，
	// 只用 slug 的话第二次归档会覆盖掉第一次。
	dst := filepath.Join(dir, fmt.Sprintf("%s.%s.md", e.Slug, now.Format("20060102")))
	for i := 2; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = filepath.Join(dir, fmt.Sprintf("%s.%s-%d.md", e.Slug, now.Format("20060102"), i))
	}
	if err := os.Rename(src, dst); err != nil {
		return Entry{}, fmt.Errorf(prompt.T(s.lang, "prompt.memory.err.archive", "归档记忆失败: %w"), err)
	}
	s.dirty = true
	return e, nil
}

// writeLocked 按当前字段重写一条已存在记忆的文件。调用方需持有写锁。
func (s *Store) writeLocked(e Entry) error {
	p, err := s.path(e.Slug)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.lang, p, render(e)); err != nil {
		return err
	}
	s.dirty = true
	return nil
}

// sameDay 判断两个时刻是否落在同一个本地日期。
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// ---------- 文件读写 ----------

// frontmatter 是记忆文件头部的 YAML 结构。时间用字符串承载，格式不合法时退回文件时间而不是报错。
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Type        string `yaml:"type"`
	Created     string `yaml:"created"`
	Updated     string `yaml:"updated"`
	// 以下三项都带 omitempty：不参与遗忘的记忆（绝大多数）文件头里不该多出
	// 三行恒为假的字段，这些文件是给人直接看和改的。
	LastUsed string `yaml:"last_used,omitempty"`
	Decay    bool   `yaml:"decay,omitempty"`
	Blurred  bool   `yaml:"blurred,omitempty"`
}

// read 解析一个记忆文件。缺少或损坏 frontmatter 时按纯 Markdown 兜底，
// 让用户手工丢进目录的普通笔记也能被索引到。
func (s *Store) read(fname string) (Entry, error) {
	p := filepath.Join(s.dir, fname)
	raw, err := os.ReadFile(p)
	if err != nil {
		return Entry{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return Entry{}, err
	}

	slug := strings.TrimSuffix(fname, ".md")
	text := normalizeText(raw)
	fm, body := splitFrontmatter(text)

	e := Entry{
		Slug:    slug,
		Name:    slug,
		Type:    defaultType,
		Content: strings.TrimSpace(body),
		Created: fi.ModTime(),
		Updated: fi.ModTime(),
	}
	if fm != nil {
		if fm.Name != "" {
			e.Name = strings.TrimSpace(fm.Name)
		}
		e.Description = strings.TrimSpace(fm.Description)
		if fm.Type != "" {
			e.Type = strings.TrimSpace(fm.Type)
		}
		if t, err := time.Parse(time.RFC3339, fm.Created); err == nil {
			e.Created = t
		}
		if t, err := time.Parse(time.RFC3339, fm.Updated); err == nil {
			e.Updated = t
		}
		e.Decay = fm.Decay
		e.Blurred = fm.Blurred
		if t, err := time.Parse(time.RFC3339, fm.LastUsed); err == nil {
			e.LastUsed = t
		}
	}
	if e.LastUsed.IsZero() {
		e.LastUsed = e.Updated // 旧文件没有该字段，从最后一次改动起算
	}
	if e.Description == "" {
		e.Description = truncateRunes(firstLine(e.Content), descMaxRunes)
	}
	e.Description = truncateRunes(collapseSpace(e.Description), descMaxRunes)
	return e, nil
}

// normalizeText 去掉 UTF-8 BOM 并把行尾统一为 LF（手工编辑的文件多为 CRLF）。
func normalizeText(raw []byte) string {
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// splitFrontmatter 只把首行的 --- 当作围栏起点，正文里的分隔线不会被误判。
// 没有合法围栏时返回 nil 与原文。
func splitFrontmatter(text string) (*frontmatter, string) {
	if !strings.HasPrefix(text, "---\n") {
		return nil, text
	}
	head, body, ok := strings.Cut(text[len("---\n"):], "\n---")
	if !ok {
		return nil, text
	}
	body = strings.TrimPrefix(body, "\n")

	var fm frontmatter
	if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
		return nil, text
	}
	return &fm, body
}

// render 生成记忆文件内容（LF 行尾）。
func render(e Entry) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	fm := frontmatter{
		Name:        e.Name,
		Description: e.Description,
		Type:        e.Type,
		Created:     e.Created.Format(time.RFC3339),
		Updated:     e.Updated.Format(time.RFC3339),
		Decay:       e.Decay,
		Blurred:     e.Blurred,
	}
	// last_used 只在与 updated 不同时写出，省掉绝大多数文件里的一行重复信息
	if !e.LastUsed.IsZero() && !e.LastUsed.Equal(e.Updated) {
		fm.LastUsed = e.LastUsed.Format(time.RFC3339)
	}
	_ = enc.Encode(fm)
	_ = enc.Close()
	b.WriteString("---\n\n")
	b.WriteString(e.Content)
	b.WriteString("\n")
	return []byte(b.String())
}

// writeFileAtomic 先写临时文件再改名，避免进程中断留下半截内容。
func writeFileAtomic(lang, path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf(prompt.T(lang, "prompt.memory.err.save", "保存记忆失败: %w"), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(lang, "prompt.memory.err.save", "保存记忆失败: %w"), err)
	}
	return nil
}

// ---------- 文件名规范化 ----------

// winReserved 是 Windows 上不能作为文件名的设备名（带扩展名同样不可用）。
var winReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// slugify 把标题折成可跨平台安全落盘的文件名：保留字母数字（含中文）与 - _，
// 其余一律折成连字符，再处理 Windows 的保留名与结尾字符限制。
func slugify(lang, name string) (string, error) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	slug := b.String()
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	// Windows 会静默剥掉结尾的点与空格，留着会让两个不同标题指向同一个文件
	slug = strings.Trim(slug, "-_. ")
	slug = truncateRunes(slug, slugMaxRunes)
	slug = strings.Trim(slug, "-_. ")

	if slug == "" {
		return "", fmt.Errorf(prompt.T(lang, "prompt.memory.err.no_usable_chars", "记忆标题 %q 不含任何可用字符"), name)
	}
	// 落盘时会补上 .md，而 Windows 把 CON.md 一样当成设备名
	if winReserved[strings.ToLower(slug)] {
		slug = "_" + slug
	}
	return slug, nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max]), " ")
}

// collapseSpace 把换行与连续空白压成单个空格，保证一条钩子只占索引里的一行。
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
