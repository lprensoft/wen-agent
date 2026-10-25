package growth

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wen/internal/prompt"
)

// 一个可见域的成长：枝叶、已认可的核心、一条待批准的提议，以及撤销用的最近改动。
//
// 落两个文件：state.json 是现状（每次原子重写），log.jsonl 是只追加的成长记录（给人看，
// 设置页的「查看成长记录」读它）。撤销靠 state 里的 History 而不是回放日志——日志是
// 审计用的，不该被「撤销」改写；而撤销只需要最近几次改动的前后两份。

// 层与操作的落盘标识（不翻译，只给程序与日志用）。
const (
	layerDetail = "detail"
	layerCore   = "core"

	opAdd     = "add"
	opModify  = "modify"
	opLetGo   = "let_go"
	opPropose = "propose"
	opApprove = "approve"
	opReject  = "reject"
	opUndo    = "undo"
	opClear   = "clear"
)

// historyCap 是可撤销的最近改动条数。
const historyCap = 20

// Entry 是一条成长：一句话，加上依据与「为什么这还是我」。
type Entry struct {
	ID    string    `json:"id"`
	Text  string    `json:"text"`
	Basis string    `json:"basis"`
	Why   string    `json:"why"`
	Since time.Time `json:"since"`
}

// Quota 是某个逻辑日里已经用掉的枝叶改动次数。
type Quota struct {
	Date string `json:"date,omitempty"`
	Used int    `json:"used,omitempty"`
}

// Change 是一次可撤销的改动：改之前与改之后（新增时 Before 为空，放下时 After 为空）。
type Change struct {
	At     time.Time `json:"at"`
	Layer  string    `json:"layer"`
	Op     string    `json:"op"`
	Before *Entry    `json:"before,omitempty"`
	After  *Entry    `json:"after,omitempty"`
}

// State 是一个可见域的成长现状。
type State struct {
	Details []Entry `json:"details,omitempty"` // 枝叶，按加入顺序
	Core    []Entry `json:"core,omitempty"`    // 已认可的核心
	Pending *Entry  `json:"pending,omitempty"` // 等待批准的核心提议，同一时间至多一条
	NextID  int     `json:"next_id,omitempty"`

	// LastReflect 是上次反思所在的逻辑日；Dirty 表示那之后有过真人对话——没人来过的
	// 日子没有新东西可想，不必为它付一轮模型调用。
	LastReflect string `json:"last_reflect,omitempty"`
	Dirty       bool   `json:"dirty,omitempty"`

	Quota   Quota    `json:"quota,omitempty"`
	History []Change `json:"history,omitempty"` // 最新在后
}

// newID 发一个新编号：枝叶以 d 开头、核心以 c 开头，模型按它点名修改或放下。
func (s *State) newID(layer string) string {
	s.NextID++
	if layer == layerCore {
		return fmt.Sprintf("c%d", s.NextID)
	}
	return fmt.Sprintf("d%d", s.NextID)
}

func (s *State) pushHistory(c Change) {
	s.History = append(s.History, c)
	if len(s.History) > historyCap {
		s.History = s.History[len(s.History)-historyCap:]
	}
}

// detailIndex 按编号找枝叶，找不到返回 -1。
func (s *State) detailIndex(id string) int {
	for i, e := range s.Details {
		if e.ID == id {
			return i
		}
	}
	return -1
}

// LogEntry 是成长记录里的一行。
type LogEntry struct {
	At    time.Time `json:"at"`
	Layer string    `json:"layer"`
	Op    string    `json:"op"`
	Text  string    `json:"text,omitempty"`
	Prev  string    `json:"prev,omitempty"` // 修改前的说法
	Basis string    `json:"basis,omitempty"`
	Why   string    `json:"why,omitempty"`
}

// Store 管理一个可见域目录下的两个文件。每次操作重新读取，单把锁串行化读改写。
type Store struct {
	mu   sync.Mutex
	lang string // 错误会经工具返回给模型，跟着进程语言走
	dir  string
}

func NewStore(lang, dir string) *Store { return &Store{lang: lang, dir: dir} }

func (s *Store) statePath() string { return filepath.Join(s.dir, "state.json") }
func (s *Store) logPath() string   { return filepath.Join(s.dir, "log.jsonl") }

// Load 读现状；文件不存在时返回零值。
func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (State, error) {
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, fmt.Errorf(prompt.T(s.lang, "prompt.growth.store.read_failed", "读取成长记录失败: %w"), err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, fmt.Errorf(prompt.T(s.lang, "prompt.growth.store.corrupt", "成长记录文件损坏: %w"), err)
	}
	return st, nil
}

// Update 在锁内读、改、写。fn 返回 false 表示不必写回；返回的 error 原样交回调用方，
// 不写盘。logs 是这次要追加进成长记录的行，写回成功之后才追加。
func (s *Store) Update(fn func(st *State) (changed bool, logs []LogEntry, err error)) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return State{}, err
	}
	changed, logs, err := fn(&st)
	if err != nil || !changed {
		return st, err
	}
	if err := s.saveLocked(st); err != nil {
		return State{}, err
	}
	s.appendLogLocked(logs)
	return st, nil
}

// saveLocked 原子写回，权限 0600：成长是对话内容的一部分。
func (s *Store) saveLocked(st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.growth.store.save_failed", "保存成长记录失败: %w"), err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.growth.store.save_failed", "保存成长记录失败: %w"), err)
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.growth.store.save_failed", "保存成长记录失败: %w"), err)
	}
	if err := os.Rename(tmp, s.statePath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.growth.store.save_failed", "保存成长记录失败: %w"), err)
	}
	return nil
}

// appendLogLocked 追加成长记录。失败只丢日志行，现状已经写好，不回滚。
func (s *Store) appendLogLocked(logs []LogEntry) {
	if len(logs) == 0 {
		return
	}
	f, err := os.OpenFile(s.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, l := range logs {
		_ = enc.Encode(l)
	}
}

// ReadLog 读成长记录的最后 n 行，最新在后。文件不存在时返回空。
func (s *Store) ReadLog(n int) ([]LogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.logPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []LogEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var l LogEntry
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, sc.Err()
}

// Clear 删掉现状并记一行「清空」。成长记录本身保留：那是发生过的事。
func (s *Store) Clear(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.statePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.appendLogLocked([]LogEntry{{At: now, Op: opClear}})
	return nil
}
