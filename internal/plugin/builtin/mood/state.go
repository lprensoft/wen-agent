// Package mood 提供心情的系统插件：角色有一个此刻的心情值，由模型按互动的演绎
// 增减，并随时间向平静回归；当前心情每轮注入上下文，影响表达方式。
//
// 插件只维护数值与回归，不写台词——心情怎么体现在说话与动作上由模型按角色决定。
//
// 本插件依赖 roleplay：没有角色，就没有「谁的心情」。
package mood

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

const (
	// 心情的量程。0 为平静，越高越开心、越低越沮丧。
	moodMin = -100
	moodMax = 100

	// maxReasonRunes 限制起因的长度。它由模型写入又逐轮进 system 消息，
	// 是一条从工具参数直通系统提示词的通路，必须压掉换行并限死长度。
	maxReasonRunes = 40
)

// State 是一个可见域里角色此刻的心情。
type State struct {
	Value   int       `json:"value"`
	Reason  string    `json:"reason,omitempty"`
	Updated time.Time `json:"updated"`
}

// Store 管理一个可见域的心情。单个 JSON 文件，每次操作重新读取不做缓存。
type Store struct {
	mu sync.Mutex
	// lang 是进程语言。这个库的错误会经工具返回给模型，因此要跟着语言走。
	lang string
	dir  string
	path string
}

// NewStore 建立指向 <dir>/mood.json 的心情库。
func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, dir: dir, path: filepath.Join(dir, "mood.json")}
}

// Current 返回按回归速率折算到此刻的心情。第二个返回值表示是否有过记录——
// 没有记录时不注入，由提示词里的「按平静起算」兜住。
func (s *Store) Current(decayPerHour int, now time.Time) (State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok, err := s.load()
	if err != nil || !ok {
		return State{}, false, err
	}
	return decay(st, decayPerHour, now), true, nil
}

// Apply 先把心情按回归速率折算到此刻，再叠加这一次的变化量。
// 折算放在叠加之前：不然隔了一天再调整，会从一天前那个还没回落的值上继续加。
//
// 折算的结果只在写入时落盘，读取路径不回写——读一次写一次盘没有必要，
// 而且会让并发的读把彼此的时间戳覆盖掉。
func (s *Store) Apply(delta, maxDelta, decayPerHour int, reason string, now time.Time) (State, int, error) {
	if delta == 0 {
		return State{}, 0, fmt.Errorf("%s", prompt.T(s.lang, "prompt.mood.result.zero_delta", "变化量不能为 0"))
	}
	applied := clamp(delta, -maxDelta, maxDelta)

	s.mu.Lock()
	defer s.mu.Unlock()
	st, _, err := s.load()
	if err != nil {
		return State{}, 0, err
	}

	out := State{
		Value:   clamp(decay(st, decayPerHour, now).Value+applied, moodMin, moodMax),
		Reason:  clipReason(reason),
		Updated: now,
	}
	if err := s.save(out); err != nil {
		return State{}, 0, err
	}
	return out, applied, nil
}

// Clear 抹掉本库的心情记录。文件不存在时不算错误。
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf(prompt.T(s.lang, "prompt.mood.store.reset_failed", "重置心情失败: %w"), err)
	}
	return nil
}

func (s *Store) load() (State, bool, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, false, nil
		}
		return State{}, false, fmt.Errorf(prompt.T(s.lang, "prompt.mood.store.read_failed", "读取心情失败: %w"), err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, false, fmt.Errorf(prompt.T(s.lang, "prompt.mood.store.corrupt", "心情记录文件损坏: %w"), err)
	}
	st.Value = clamp(st.Value, moodMin, moodMax) // 手改过的文件不该把量程撑破
	return st, true, nil
}

// save 原子写回，权限 0600：心情记录属于对话内容的一部分。
func (s *Store) save(st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.mood.store.save_failed", "保存心情失败: %w"), err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.mood.store.mkdir_failed", "创建心情目录失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.mood.store.save_failed", "保存心情失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.mood.store.save_failed", "保存心情失败: %w"), err)
	}
	return nil
}

// decay 把心情按每小时若干点向 0 靠拢。真人不会把一次不快原样带到三天以后。
//
// 回归量由落盘时记下的时间戳与当前时间作差算得，不含推测；时间戳缺失或倒流
// （改过系统时钟）时原样返回，不猜。回落到 0 时连起因一起丢掉——心情都平了，
// 那件事就不该再挂在提示词里。
func decay(st State, perHour int, now time.Time) State {
	if perHour <= 0 || st.Value == 0 || st.Updated.IsZero() {
		return st
	}
	elapsed := now.Sub(st.Updated)
	if elapsed <= 0 {
		return st
	}
	drop := int(float64(perHour) * elapsed.Hours())
	if drop <= 0 {
		return st
	}
	if st.Value > 0 {
		st.Value = max(st.Value-drop, 0)
	} else {
		st.Value = min(st.Value+drop, 0)
	}
	if st.Value == 0 {
		st.Reason = ""
	}
	return st
}

// clipReason 规整起因：压掉换行与连续空白，超长截断。
// 这里截断而不报错——起因只是给演绎承接用的辅助信息，为它中断一次调整不值得。
func clipReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if r := []rune(reason); len(r) > maxReasonRunes {
		return string(r[:maxReasonRunes])
	}
	return reason
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }
