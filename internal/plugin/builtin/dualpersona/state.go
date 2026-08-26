package dualpersona

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"wen/internal/i18n"
)

// maxTrackedSessions 限制状态文件里记录的会话数，超出后丢弃最早写入的。
// 这个文件每轮对话都要重写，不能让它随会话数无限增长。
const maxTrackedSessions = 500

// state 是落盘的人格状态。
//
// 它是权威来源，历史只做兜底：压缩会按可见域重排消息、外部编辑也可能改动 jsonl，
// 靠「最后一条带标签的消息」反推会在这些情况下给出错误答案。
type state struct {
	// Last 是最近一次对话所处的人格，新会话据此继承。
	Last string `json:"last"`
	// Sessions 记录每个会话各自停在哪个人格，切回旧会话时按它恢复。
	Sessions map[string]string `json:"sessions,omitempty"`
	// Order 是 Sessions 的写入顺序，用于超出上限时丢弃最早的。
	Order []string `json:"order,omitempty"`
}

// store 读写状态文件。两个会话可能同时在对话，全部操作加锁并原子写。
type store struct {
	mu   sync.Mutex
	path string
	st   state
}

func newStore(lang, dir string) (*store, error) {
	if dir == "" {
		return nil, fmt.Errorf("%s", i18n.T(lang, "plugin.dual_persona.no_state_dir",
			"没有可用的持久化目录，无法记住人格状态"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.dual_persona.err.mkdir",
			"创建状态目录失败: %w"), err)
	}
	s := &store{path: filepath.Join(dir, "state.json")}
	s.st = s.load()
	return s, nil
}

// load 读取状态文件。读不到或解析失败一律当作「没有记录」：状态是可再生的，
// 不该因为一个坏文件让插件无法启用。
func (s *store) load() state {
	st := state{Sessions: map[string]string{}}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return st
	}
	var got state
	if err := json.Unmarshal(raw, &got); err != nil {
		return st
	}
	if got.Sessions == nil {
		got.Sessions = map[string]string{}
	}
	return got
}

// personaFor 返回该会话应当处于的人格：先看这个会话自己的记录，
// 没有则继承上一次对话的人格（这就是「新建会话延续上次状态」），最后退到表人格。
func (s *store) personaFor(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if p, ok := s.st.Sessions[sessionID]; ok && valid(p) {
		return p
	}
	if valid(s.st.Last) {
		return s.st.Last
	}
	return personaOuter
}

// remember 记下该会话当前的人格并落盘。写失败只影响下次重启后的恢复，
// 不该让本轮对话失败，因此由调用方决定是否记日志。
func (s *store) remember(sessionID, persona string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.st.Sessions == nil {
		s.st.Sessions = map[string]string{}
	}
	if _, exists := s.st.Sessions[sessionID]; !exists {
		s.st.Order = append(s.st.Order, sessionID)
	}
	s.st.Sessions[sessionID] = persona
	s.st.Last = persona
	s.trimLocked()
	return s.saveLocked()
}

// trimLocked 把记录数压回上限，丢弃最早写入的会话。
func (s *store) trimLocked() {
	if len(s.st.Order) <= maxTrackedSessions {
		return
	}
	drop := s.st.Order[:len(s.st.Order)-maxTrackedSessions]
	for _, id := range drop {
		delete(s.st.Sessions, id)
	}
	s.st.Order = append([]string(nil), s.st.Order[len(drop):]...)
}

func (s *store) saveLocked() error {
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	// 0600：里面记录着用户处在哪个人格，属于隐私内容
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
