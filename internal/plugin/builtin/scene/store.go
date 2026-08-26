// Package scene 提供场景感知的系统插件：注入用户配置的场景与环境设定作为演绎的舞台，
// 并引导模型把对话中出现的场景与地理位置记录成场景记忆，使场景跨轮次、跨会话延续。
//
// 本插件依赖 roleplay：舞台是为角色扮演搭的，没有角色，场景设定与场景记忆都没有
// 作用对象。
package scene

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
	// maxNameRunes / maxDetailRunes 限制单条场景的体量：场景记忆会随每轮对话全文注入，
	// 单条必须有确定上界。超长时报错让模型压缩后重试，而不是悄悄截断——被截掉的
	// 后半段模型不知道丢了，之后的演绎会依据一份残缺的场景。
	maxNameRunes   = 30
	maxDetailRunes = 500
)

// Scene 是一条场景记忆：一个地点或场景的名称与描述。
type Scene struct {
	Name    string    `json:"name"`
	Detail  string    `json:"detail"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Domain 是这条场景所属的可见域标签，不落盘：它由所在的库决定，
	// 跨库合并后用于把 delete 送回正确的库。
	Domain string `json:"-"`
}

// Store 管理一个可见域的场景库：单个 JSON 文件，条目保持记录顺序。
// 文件可能被用户在进程外修改，因此每次操作都重新读取，不做缓存——
// 场景条目数量小，读取开销可以忽略。
type Store struct {
	mu sync.Mutex
	// lang 是进程语言。这个库的错误会经工具返回给模型，因此要跟着语言走。
	lang string
	path string
}

// NewStore 建立指向 <dir>/scenes.json 的场景库。
func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, path: filepath.Join(dir, "scenes.json")}
}

// List 返回全部场景，按记录顺序。文件不存在时返回空列表。
func (s *Store) List() ([]Scene, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() ([]Scene, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.scene.store.read_failed", "读取场景记忆失败: %w"), err)
	}
	var scenes []Scene
	if err := json.Unmarshal(raw, &scenes); err != nil {
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.scene.store.corrupt", "场景记忆文件损坏: %w"), err)
	}
	return scenes, nil
}

// Save 记录一条场景。replace 为 false 时拒绝覆盖同名条目；覆盖保留原记录时间与
// 原有位置——场景的先后顺序承载着舞台展开的脉络，更新描述不应把它挪到末尾。
func (s *Store) Save(in Scene, replace bool) (Scene, error) {
	name, err := normalizeName(s.lang, in.Name)
	if err != nil {
		return Scene{}, err
	}
	detail := strings.TrimSpace(in.Detail)
	if detail == "" {
		return Scene{}, fmt.Errorf("%s", prompt.T(s.lang, "prompt.scene.store.detail_empty", "场景描述不能为空"))
	}
	if n := len([]rune(detail)); n > maxDetailRunes {
		return Scene{}, fmt.Errorf(prompt.T(s.lang, "prompt.scene.store.detail_long",
			"场景描述过长（%d 字，上限 %d 字），请压缩后重试"), n, maxDetailRunes)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	scenes, err := s.load()
	if err != nil {
		return Scene{}, err
	}

	now := time.Now()
	out := Scene{Name: name, Detail: detail, Created: now, Updated: now}
	if i := indexOf(scenes, name); i >= 0 {
		if !replace {
			return Scene{}, fmt.Errorf(prompt.T(s.lang, "prompt.scene.store.exists",
				"已记录过场景 %q，如需更新请把 mode 设为 replace"), scenes[i].Name)
		}
		out.Created = scenes[i].Created
		scenes[i] = out
	} else {
		scenes = append(scenes, out)
	}
	if err := s.save(scenes); err != nil {
		return Scene{}, err
	}
	return out, nil
}

// Delete 删除一条场景。
func (s *Store) Delete(name string) (Scene, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scenes, err := s.load()
	if err != nil {
		return Scene{}, err
	}
	i := indexOf(scenes, name)
	if i < 0 {
		return Scene{}, fmt.Errorf(prompt.T(s.lang, "prompt.scene.result.not_found",
			"没有名为 %q 的场景"), strings.TrimSpace(name))
	}
	out := scenes[i]
	scenes = append(scenes[:i], scenes[i+1:]...)
	if err := s.save(scenes); err != nil {
		return Scene{}, err
	}
	return out, nil
}

// save 原子写回整个文件，避免进程中断留下半截内容。
func (s *Store) save(scenes []Scene) error {
	if scenes == nil {
		scenes = []Scene{}
	}
	raw, err := json.MarshalIndent(scenes, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.scene.store.save_failed", "保存场景记忆失败: %w"), err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.scene.store.mkdir_failed", "创建场景记忆目录失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.scene.store.save_failed", "保存场景记忆失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.scene.store.save_failed", "保存场景记忆失败: %w"), err)
	}
	return nil
}

// indexOf 按名称查找场景（大小写不敏感）。
func indexOf(scenes []Scene, name string) int {
	want := strings.ToLower(strings.TrimSpace(name))
	for i, sc := range scenes {
		if strings.ToLower(sc.Name) == want {
			return i
		}
	}
	return -1
}

// normalizeName 规整场景名称：压掉换行与连续空白，校验非空与长度。
func normalizeName(lang, name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.scene.store.name_empty", "场景名称不能为空"))
	}
	if n := len([]rune(name)); n > maxNameRunes {
		return "", fmt.Errorf(prompt.T(lang, "prompt.scene.store.name_long",
			"场景名称过长（%d 字，上限 %d 字），请用更短的名称"), n, maxNameRunes)
	}
	return name, nil
}
