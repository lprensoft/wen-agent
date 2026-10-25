package bookshelf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wen/internal/prompt"
)

// 印象的上限。印象随每轮注入，单条与条数都要有确定上界。
const (
	maxNotes     = 5  // 每本书留最近几条
	maxNoteRunes = 60 // 一条多长
)

// Note 是读书时记下的一句印象，连同记下时读到了几成——「读到一成时觉得他是坏人」与
// 「读完觉得他是坏人」是两回事。
type Note struct {
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Percent int       `json:"percent"`
}

// Progress 是一本书的进度。Offset 是下一段的起点（解码后正文的字节偏移），Length 是
// 上次读时全书的长度——文件被换过时偏移只能按新长度截住，不做更聪明的对齐。
type Progress struct {
	Offset   int       `json:"offset"`
	Length   int       `json:"length"`
	Started  time.Time `json:"started"`
	LastRead time.Time `json:"last_read"`
	Finished time.Time `json:"finished,omitempty"`
	Notes    []Note    `json:"notes,omitempty"`
}

// State 是一个可见域里的全部读书进度。Current 是最近读的那本（文件名）。
type State struct {
	Current string               `json:"current,omitempty"`
	Books   map[string]*Progress `json:"books,omitempty"`
}

// Store 是一个可见域的进度库。进度属于人格：表里两面各读各的，一面读过的另一面不知道。
type Store struct {
	mu   sync.Mutex
	lang string
	dir  string
	path string
}

func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, dir: dir, path: filepath.Join(dir, "progress.json")}
}

// Load 读出当前进度。没有文件时返回空状态。
func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Update 在锁内读出、修改、写回。fn 返回错误时不写盘。
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	return s.save(st)
}

func (s *Store) load() (State, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{Books: map[string]*Progress{}}, nil
		}
		return State{}, fmt.Errorf(prompt.T(s.lang, "prompt.bookshelf.err.load", "读取读书进度失败: %w"), err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, fmt.Errorf(prompt.T(s.lang, "prompt.bookshelf.err.load", "读取读书进度失败: %w"), err)
	}
	if st.Books == nil {
		st.Books = map[string]*Progress{}
	}
	return st, nil
}

// save 原子写回，权限 0600：读过什么、怎么想，属于角色的私事。
func (s *Store) save(st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.bookshelf.err.save", "保存读书进度失败: %w"), err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.bookshelf.err.save", "保存读书进度失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.bookshelf.err.save", "保存读书进度失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.bookshelf.err.save", "保存读书进度失败: %w"), err)
	}
	return nil
}

// addNote 追加一条印象，超出条数时挤掉最早的。
func addNote(p *Progress, n Note) {
	p.Notes = append(p.Notes, n)
	if len(p.Notes) > maxNotes {
		p.Notes = append([]Note(nil), p.Notes[len(p.Notes)-maxNotes:]...)
	}
}
