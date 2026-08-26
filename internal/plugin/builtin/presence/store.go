// Package presence 提供现场状态的系统插件：一份结构化的「当下」快照——所在地点、
// 穿着、姿态、正在做的事、持续生效的状态、感官焦点——由模型在演绎推进时随手更新，
// 每轮注入上下文。
//
// 它解决的是连续性问题：场景与姿态此前只存在于历史里最近一段【】演绎中，上下文
// 一长、一压缩，或者换个会话，穿着与位置就开始漂移。快照落盘在会话之外，天然跨
// 会话、跨压缩接力，与 roleplay 压缩时抽取最后一处【】的兜底互补。
//
// 本插件依赖 roleplay：快照描述的是角色的现场，没有角色就没有作用对象。
package presence

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

// fieldDef 描述快照的一个字段：键名、注入时的标签、长度上限。
// 上限各不相同：字段内容由模型写入又逐轮进上下文，必须压掉换行并限死长度。
type fieldDef struct {
	key      string
	label    prompt.Piece
	maxRunes int
}

// fieldDefs 是快照的全部字段（渲染按此顺序）。
var fieldDefs = []fieldDef{
	{"location", prompt.Text("prompt.presence.field.location", "所在"), 60},
	{"attire", prompt.Text("prompt.presence.field.attire", "穿着"), 120},
	{"posture", prompt.Text("prompt.presence.field.posture", "姿态"), 120},
	{"activity", prompt.Text("prompt.presence.field.activity", "在做"), 120},
	{"effects", prompt.Text("prompt.presence.field.effects", "持续状态"), 200},
	{"focus", prompt.Text("prompt.presence.field.focus", "感官焦点"), 80},
}

func defOf(key string) (fieldDef, bool) {
	for _, d := range fieldDefs {
		if d.key == key {
			return d, true
		}
	}
	return fieldDef{}, false
}

// Field 是快照里的一项：内容加落盘时间（注入时给出「多久前更新」的线索）。
type Field struct {
	Text    string    `json:"text"`
	Updated time.Time `json:"updated"`
}

// Snapshot 按字段键存放快照。用 map 而不是定型结构：读写两侧都按 fieldDefs
// 驱动，将来加字段只改那张表。
type Snapshot map[string]Field

// Store 管理一个可见域的快照。单个 JSON 文件，每次操作重新读取不做缓存。
type Store struct {
	mu sync.Mutex
	// lang 是进程语言。字段标签与这里的错误都经工具返回给模型，要跟着语言走。
	lang string
	dir  string
	path string
}

// NewStore 建立指向 <dir>/presence.json 的快照库。
func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, dir: dir, path: filepath.Join(dir, "presence.json")}
}

// Load 返回当前快照。第二个返回值表示是否有过记录。
func (s *Store) Load() (Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Apply 应用一批字段变更：非空值更新该字段，空串清除该字段。
// 未知的键报错——工具层已按 schema 约束，走到这说明参数拼错了。
// 返回实际更新与清除的字段标签，供工具回显。
func (s *Store) Apply(changes map[string]string, now time.Time) (updated, cleared []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap, _, err := s.load()
	if err != nil {
		return nil, nil, err
	}
	if snap == nil {
		snap = Snapshot{}
	}
	// 按 fieldDefs 的顺序处理，回显的字段顺序才稳定
	for _, d := range fieldDefs {
		v, ok := changes[d.key]
		if !ok {
			continue
		}
		delete(changes, d.key)
		if strings.TrimSpace(v) == "" {
			if _, had := snap[d.key]; had {
				delete(snap, d.key)
				cleared = append(cleared, d.label.For(s.lang))
			}
			continue
		}
		snap[d.key] = Field{Text: clipText(v, d.maxRunes), Updated: now}
		updated = append(updated, d.label.For(s.lang))
	}
	for k := range changes {
		return nil, nil, fmt.Errorf(prompt.T(s.lang, "prompt.presence.unknown_field", "未知的字段 %q"), k)
	}
	if len(updated) == 0 && len(cleared) == 0 {
		return nil, nil, nil // 没有实际变化，不写盘
	}
	if len(snap) == 0 {
		// 全部字段都清掉了：删文件而不是留一个空对象
		if err := s.remove(); err != nil {
			return nil, nil, err
		}
		return updated, cleared, nil
	}
	return updated, cleared, s.save(snap)
}

// Clear 抹掉本库的快照。文件不存在时不算错误。
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remove()
}

func (s *Store) remove() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf(prompt.T(s.lang, "prompt.presence.store.clear_failed", "清除现场状态失败: %w"), err)
	}
	return nil
}

func (s *Store) load() (Snapshot, bool, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf(prompt.T(s.lang, "prompt.presence.store.read_failed", "读取现场状态失败: %w"), err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, false, fmt.Errorf(prompt.T(s.lang, "prompt.presence.store.corrupt", "现场状态文件损坏: %w"), err)
	}
	// 手改过的文件不该把长度上限撑破
	for k, f := range snap {
		d, ok := defOf(k)
		if !ok || strings.TrimSpace(f.Text) == "" {
			delete(snap, k)
			continue
		}
		f.Text = clipText(f.Text, d.maxRunes)
		snap[k] = f
	}
	return snap, len(snap) > 0, nil
}

// save 原子写回，权限 0600：现场快照属于对话内容的一部分。
func (s *Store) save(snap Snapshot) error {
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.presence.store.save_failed", "保存现场状态失败: %w"), err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.presence.store.mkdir_failed", "创建现场状态目录失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.presence.store.save_failed", "保存现场状态失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.presence.store.save_failed", "保存现场状态失败: %w"), err)
	}
	return nil
}

// clipText 规整字段内容：压掉换行与连续空白，超长截断。
// 截断而不报错——快照是演绎的辅助信息，为它中断一次更新不值得。
func clipText(text string, maxRunes int) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > maxRunes {
		return string(r[:maxRunes])
	}
	return text
}
