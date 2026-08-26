package bodysense

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

// maxActRunes 限制「方式」的长度。这个字段由模型写入，又逐轮进 system 消息，
// 是一条从工具参数直通系统提示词的通路，必须压掉换行并限死长度。
const maxActRunes = 20

// PartState 是一个部位的累计接触状态。
type PartState struct {
	Part    string    `json:"part"`
	Count   int       `json:"count"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
	LastAct string    `json:"last_act,omitempty"`
}

// Touch 是一次上报的接触。
type Touch struct {
	Part   string
	Action string
}

// Store 管理一个可见域的接触记录：单个 JSON 文件，条目保持首次记录的顺序。
// 文件可能被用户在进程外修改，因此每次操作都重新读取，不做缓存——部位数天然有界，
// 读取开销可以忽略。
//
// 跨进程并发（两个实例指向同一配置目录）会丢更新，与 scene / memory 一样，
// 不为此自造文件锁。
type Store struct {
	mu sync.Mutex
	// lang 是进程语言。这个库的错误会经工具返回给模型，因此要跟着语言走。
	lang string
	dir  string
	path string
}

// NewStore 建立指向 <dir>/body.json 的记录库。
func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, dir: dir, path: filepath.Join(dir, "body.json")}
}

// List 返回全部有记录的部位，按首次记录的顺序。文件不存在时返回空列表。
func (s *Store) List() ([]PartState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() ([]PartState, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.read_failed", "读取接触记录失败: %w"), err)
	}
	var states []PartState
	if err := json.Unmarshal(raw, &states); err != nil {
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.corrupt", "接触记录文件损坏: %w"), err)
	}
	return states, nil
}

// Record 累加若干次接触，返回本库内更新后的状态（顺序与传入的部位一致）。
// 同一部位在一次调用里出现多次只算一次：计数没有回退手段，一次幻觉不该能加上几十次。
func (s *Store) Record(touches []Touch) ([]PartState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	states, err := s.load()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var (
		out  []PartState
		done = map[string]bool{}
	)
	for _, t := range touches {
		if done[t.Part] {
			continue
		}
		done[t.Part] = true

		act := clipAct(t.Action)
		i := indexOf(states, t.Part)
		if i < 0 {
			states = append(states, PartState{Part: t.Part, Count: 1, First: now, Last: now, LastAct: act})
			i = len(states) - 1
		} else {
			states[i].Count++
			states[i].Last = now
			if states[i].First.IsZero() {
				states[i].First = now
			}
			if act != "" {
				states[i].LastAct = act
			}
		}
		out = append(out, states[i])
	}

	if err := s.save(states); err != nil {
		return nil, err
	}
	return out, nil
}

// Clear 清空本库。文件不存在时不算错误。
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.clear_failed", "清空接触记录失败: %w"), err)
	}
	return nil
}

// save 原子写回整个文件，避免进程中断留下半截内容。
// 权限比 scene 严一档（0700/0600）：这份数据记录的是身体接触的累计情况，
// 敏感度不低于同样用 0600 的 plugins.state.json。
func (s *Store) save(states []PartState) error {
	if states == nil {
		states = []PartState{}
	}
	raw, err := json.MarshalIndent(states, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.save_failed", "保存接触记录失败: %w"), err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.mkdir_failed", "创建接触记录目录失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.save_failed", "保存接触记录失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.save_failed", "保存接触记录失败: %w"), err)
	}
	return nil
}

// indexOf 按部位名查找记录（大小写不敏感）。
func indexOf(states []PartState, part string) int {
	want := strings.ToLower(part)
	for i, st := range states {
		if strings.ToLower(st.Part) == want {
			return i
		}
	}
	return -1
}

// clipAct 规整「方式」：压掉全部空白与换行，超长截断。
// 这里截断而不报错——方式只是给演绎承接用的辅助信息，为它中断一次上报不值得。
func clipAct(act string) string {
	act = strings.Join(strings.Fields(act), "")
	if r := []rune(act); len(r) > maxActRunes {
		return string(r[:maxActRunes])
	}
	return act
}
