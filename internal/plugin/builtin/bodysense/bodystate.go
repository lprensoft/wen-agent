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

// 即时身体状态：唤起与疲劳，各 0..100，随时间向 0 回落。
//
// 它补的是接触计数（长期熟悉度）与心情之间缺的那层——「此刻身体是什么状态」。
// 回落的过程本身就是余韵与恢复，不单设状态；落盘在会话之外，跨会话天然接力。
//
// 与接触计数同一套分域规则：按可见域各存一份、写入本轮写入域，读取时对可读域
// 逐字段取最大值（每个域记下的都是身体状态的下界，方向与计数求和一样安全——
// 表人格读不到里域那份，里人格看得到全部）。不学 mood 的「各域完全独立」：
// 心情属于人格，身体是同一具。

const (
	stateMin = 0
	stateMax = 100

	// maxStateReasonRunes 限制起因长度：模型写入又逐轮进上下文的通路，限死。
	maxStateReasonRunes = 40
)

// BodyState 是一个可见域记下的即时身体状态。
type BodyState struct {
	Arousal int       `json:"arousal"`
	Fatigue int       `json:"fatigue"`
	Reason  string    `json:"reason,omitempty"`
	Updated time.Time `json:"updated"`
}

// StateStore 管理一个可见域的即时身体状态。单个 JSON 文件，每次操作重新读取。
type StateStore struct {
	mu sync.Mutex
	// lang 是进程语言。这个库的错误会经工具返回给模型，因此要跟着语言走。
	lang string
	dir  string
	path string
}

// NewStateStore 建立指向 <dir>/state.json 的状态库（与 body.json 同目录，
// 但不挤进那个文件：那边是数组、累加语义，混用会让两种语义共用一次原子写）。
func NewStateStore(lang, dir string) *StateStore {
	return &StateStore{lang: lang, dir: dir, path: filepath.Join(dir, "state.json")}
}

// Current 返回按回落速率折算到此刻的状态。第二个返回值表示是否有过记录。
func (s *StateStore) Current(arousalPerHour, fatiguePerHour int, now time.Time) (BodyState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok, err := s.load()
	if err != nil || !ok {
		return BodyState{}, false, err
	}
	return decayState(st, arousalPerHour, fatiguePerHour, now), true, nil
}

// Apply 先把状态折算到此刻，再叠加这一次的变化量（两项各自受单次上限约束）。
// 返回实际生效的两个变化量，供工具回显被收拢的事实。
func (s *StateStore) Apply(dArousal, dFatigue, maxDelta, arousalPerHour, fatiguePerHour int, reason string, now time.Time) (BodyState, int, int, error) {
	if dArousal == 0 && dFatigue == 0 {
		return BodyState{}, 0, 0, fmt.Errorf("%s", prompt.T(s.lang, "prompt.body_sense.result.both_zero",
			"两个变化量不能都为 0"))
	}
	appliedA := clampInt(dArousal, -maxDelta, maxDelta)
	appliedF := clampInt(dFatigue, -maxDelta, maxDelta)

	s.mu.Lock()
	defer s.mu.Unlock()
	st, _, err := s.load()
	if err != nil {
		return BodyState{}, 0, 0, err
	}
	cur := decayState(st, arousalPerHour, fatiguePerHour, now)
	out := BodyState{
		Arousal: clampInt(cur.Arousal+appliedA, stateMin, stateMax),
		Fatigue: clampInt(cur.Fatigue+appliedF, stateMin, stateMax),
		Reason:  clipStateReason(reason),
		Updated: now,
	}
	if out.Arousal == 0 && out.Fatigue == 0 {
		out.Reason = ""
	}
	if err := s.save(out); err != nil {
		return BodyState{}, 0, 0, err
	}
	return out, appliedA, appliedF, nil
}

// Clear 抹掉本库的状态记录。文件不存在时不算错误。
func (s *StateStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.state_clear_failed", "重置身体状态失败: %w"), err)
	}
	return nil
}

func (s *StateStore) load() (BodyState, bool, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return BodyState{}, false, nil
		}
		return BodyState{}, false, fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.state_read_failed", "读取身体状态失败: %w"), err)
	}
	var st BodyState
	if err := json.Unmarshal(raw, &st); err != nil {
		return BodyState{}, false, fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.state_corrupt", "身体状态文件损坏: %w"), err)
	}
	// 手改过的文件不该把量程撑破
	st.Arousal = clampInt(st.Arousal, stateMin, stateMax)
	st.Fatigue = clampInt(st.Fatigue, stateMin, stateMax)
	return st, true, nil
}

// save 原子写回，权限 0600：身体状态属于对话内容的一部分。
func (s *StateStore) save(st BodyState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.state_save_failed", "保存身体状态失败: %w"), err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.state_mkdir_failed", "创建身体状态目录失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.state_save_failed", "保存身体状态失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.state_save_failed", "保存身体状态失败: %w"), err)
	}
	return nil
}

// decayState 把两项各按每小时若干点向 0 回落。回落量由落盘时间戳与当前时间作差
// 算得，不含推测；时间戳缺失或倒流时原样返回。两项都落到 0 时连起因一起丢掉。
func decayState(st BodyState, arousalPerHour, fatiguePerHour int, now time.Time) BodyState {
	if st.Updated.IsZero() {
		return st
	}
	elapsed := now.Sub(st.Updated)
	if elapsed <= 0 {
		return st
	}
	st.Arousal = decayValue(st.Arousal, arousalPerHour, elapsed)
	st.Fatigue = decayValue(st.Fatigue, fatiguePerHour, elapsed)
	if st.Arousal == 0 && st.Fatigue == 0 {
		st.Reason = ""
	}
	return st
}

func decayValue(v, perHour int, elapsed time.Duration) int {
	if perHour <= 0 || v == 0 {
		return v
	}
	drop := int(float64(perHour) * elapsed.Hours())
	if drop <= 0 {
		return v
	}
	return max(v-drop, 0)
}

// clipStateReason 规整起因：压掉换行与连续空白，超长截断。
func clipStateReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if r := []rune(reason); len(r) > maxStateReasonRunes {
		return string(r[:maxStateReasonRunes])
	}
	return reason
}

func clampInt(v, lo, hi int) int { return min(max(v, lo), hi) }

// ---------- 档位词 ----------

// arousalBand / fatigueBand 按数值给出状态词，分档取 15 / 40 / 70 三道线。
// 0 一档在渲染处整项省略，不出词。
func arousalBand(lang string, v int) string {
	switch {
	case v < 15:
		return prompt.T(lang, "prompt.body_sense.arousal.calm", "平静")
	case v < 40:
		return prompt.T(lang, "prompt.body_sense.arousal.warm", "微热")
	case v < 70:
		return prompt.T(lang, "prompt.body_sense.arousal.stirred", "情动")
	default:
		return prompt.T(lang, "prompt.body_sense.arousal.deep", "深陷")
	}
}

func fatigueBand(lang string, v int) string {
	switch {
	case v < 15:
		return prompt.T(lang, "prompt.body_sense.fatigue.fresh", "精神尚好")
	case v < 40:
		return prompt.T(lang, "prompt.body_sense.fatigue.tiring", "有些乏")
	case v < 70:
		return prompt.T(lang, "prompt.body_sense.fatigue.weary", "疲惫")
	default:
		return prompt.T(lang, "prompt.body_sense.fatigue.spent", "累极")
	}
}

// renderBodyState 渲染一行当前状态。为 0 的项整个省略，两项都为 0 时返回空串
// （完全平复的身体不值得占一行）。体量固定，不需要预算与降级。
//
// 不注入「距上次多久」：回落速率已经把时间编码进数值本身，再报一次间隔是重复的。
func renderBodyState(lang string, st BodyState) string {
	var parts []string
	if st.Arousal > 0 {
		parts = append(parts, prompt.Tf(lang, "prompt.body_sense.render.arousal",
			"唤起 %s（%d）", arousalBand(lang, st.Arousal), st.Arousal))
	}
	if st.Fatigue > 0 {
		parts = append(parts, prompt.Tf(lang, "prompt.body_sense.render.fatigue",
			"疲劳 %s（%d）", fatigueBand(lang, st.Fatigue), st.Fatigue))
	}
	if len(parts) == 0 {
		return ""
	}
	out := prompt.Tf(lang, "prompt.body_sense.render.now", "此刻：%s。",
		strings.Join(parts, prompt.T(lang, "prompt.body_sense.render.comma", "，")))
	if st.Reason != "" {
		out += prompt.Tf(lang, "prompt.body_sense.render.cause", "起因：%s。", st.Reason)
	}
	return out
}
