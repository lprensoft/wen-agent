// Package people 提供人物库的系统插件：角色生活里固定出现的人——朋友、家人、同事、
// 常打交道的店家——各有名字、关系、几句设定与亲近度，随相处更新，跨会话与压缩保持，
// 并在每轮注入一份清单供演绎使用。
//
// 它存在的理由是约束而不是记录：没有人物库时模型每次都临场编人，闺蜜的名字三天换一个；
// 有了它，演绎里提到的熟人只能从库里来，新认识的人必须先登记才算存在。日程插件
// 排「和谁」时也只认这里的名字（见 Lookup）。
//
// 本插件依赖 roleplay：人物是角色的社交圈，没有角色就没有归属对象。对方本人不进库——
// 对方的信息由 roleplay 的「我的信息」承担，记忆插件负责其变化。
//
// 与相邻插件的分工：memory 记「与某人之间发生过什么」（事件），本插件只记「这个人是谁、
// 现在走得多近、上次来往是什么时候」（名册）。
package people

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
	// 单条内容的体量上限：清单会随每轮对话注入，单条必须有确定上界。超长时报错让模型
	// 压缩后重试，而不是悄悄截断——被截掉的部分模型不知道丢了。
	maxNameRunes     = 20
	maxRelationRunes = 40
	maxProfileRunes  = 200
	maxNoteRunes     = 60
	maxLatelyRunes   = 40

	// latelyTTL 是近况的有效期：听说了一个月还没更新的近况多半已经变了，不再注入，
	// 免得旧消息一直当新闻说（落盘保留，list_people 仍看得到）。
	latelyTTL = 30 * 24 * time.Hour
)

// closenessLevels 是亲近度的四档，由疏到亲。做成枚举而不是数值：模型对「熟」「亲近」
// 的把握远好于对 0-100 的把握，四档也足够表达「该不该随手约他」。
//
// 它**会原样落盘**，所以中文是规范形式：Schema 按语言给出候选项，模型报回来的值先
// 换回中文再存，换一次语言不会让同一个字段有两种写法。
var closenessLevels = prompt.NewEnum(
	prompt.Text("prompt.people.closeness.nodding", "点头之交"),
	prompt.Text("prompt.people.closeness.known", "认识"),
	prompt.Text("prompt.people.closeness.close", "熟"),
	prompt.Text("prompt.people.closeness.intimate", "亲近"),
)

const defaultCloseness = "认识"

// selfNames 是指代对方本人的称呼，登记时拒绝：对方不进人物库。
//
// 借的也是 Enum：这里要问的正是「这个字符串是不是这一组里的，不论哪门语言」——
// 英文界面下模型写的是 "you"，中文界面下是「你」，两边都得拦住。
var selfNames = prompt.NewEnum(
	prompt.Text("prompt.people.self.counterpart", "对方"),
	prompt.Text("prompt.people.self.you", "你"),
	prompt.Text("prompt.people.self.you_polite", "您"),
	prompt.Text("prompt.people.self.user", "用户"),
	prompt.Text("prompt.people.self.me", "我"),
	prompt.Text("prompt.people.self.myself", "自己"),
)

// Person 是人物库里的一个人。
type Person struct {
	Name      string `json:"name"`
	Relation  string `json:"relation"`
	Profile   string `json:"profile,omitempty"`
	Closeness string `json:"closeness"`
	// LastMet 是上次实际互动（见面、通话、一起做了件事）的时刻，零值表示没有记录。
	LastMet  time.Time `json:"last_met"`
	LastNote string    `json:"last_note,omitempty"`
	// Lately 是这个人自己最近的状况（「在闹离职」），LatelyAt 是听说的时刻。与 LastNote
	// 不同：那是角色和他之间的事，这是他自己的生活——社交圈里的人也在过日子。
	Lately   string    `json:"lately,omitempty"`
	LatelyAt time.Time `json:"lately_at,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	// Domain 是这个人所属的可见域标签，不落盘：它由所在的库决定，
	// 跨库合并后用于把后续写操作送回正确的库。
	Domain string `json:"-"`
}

// Update 是 upsert_person 的一次改动。指针为 nil 表示「这次没传」，与传空串区分开：
// 模型只传要改的字段。
type Update struct {
	Name      string
	Relation  *string
	Profile   *string
	Closeness *string
	MetNow    bool
	LastNote  *string
	Lately    *string
}

// Limits 是写入时生效的上限，来自插件配置。
type Limits struct {
	MaxPeople int
}

// UpsertResult 是一次登记或更新的回执素材。
type UpsertResult struct {
	Name    string
	Created bool
	Changes []string // 更新时各字段的变化描述，如「亲近度 熟→亲近」
}

// Store 管理一个可见域的人物库：单个 JSON 文件。文件可能被用户在进程外修改，
// 因此每次操作都重新读取，不做缓存——人数有上限，读取开销可以忽略。
type Store struct {
	mu sync.Mutex
	// lang 是进程语言。这个库的错误与回执片段会经工具返回给模型，跟着语言走。
	lang string
	path string
}

// NewStore 建立指向 <dir>/people.json 的人物库。
func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, path: filepath.Join(dir, "people.json")}
}

// List 返回全部人物，按记录顺序。文件不存在时返回空列表。
func (s *Store) List() ([]Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() ([]Person, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.people.store.read_failed", "读取人物库失败: %w"), err)
	}
	var ps []Person
	if err := json.Unmarshal(raw, &ps); err != nil {
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.people.store.corrupt", "人物库文件损坏: %w"), err)
	}
	return ps, nil
}

// Upsert 登记一个新人物或更新已有的。新建时关系必填（只有一个名字的人立不起来），
// 亲近度缺省为「认识」；更新时只动传了的字段。整批校验通过才落盘。
func (s *Store) Upsert(u Update, now time.Time, lim Limits) (UpsertResult, error) {
	name, err := normalizeName(s.lang, u.Name)
	if err != nil {
		return UpsertResult{}, err
	}
	if err := validateFields(s.lang, u); err != nil {
		return UpsertResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.load()
	if err != nil {
		return UpsertResult{}, err
	}

	i := personIndex(ps, name)
	if i < 0 {
		if u.Relation == nil || strings.TrimSpace(*u.Relation) == "" {
			return UpsertResult{}, fmt.Errorf(prompt.T(s.lang, "prompt.people.result.relation_required",
				"登记新人物 %q 时要写明 relation（是什么关系）"), name)
		}
		// 拦截生效时要把规则告诉模型，否则它只会换个名字再试一次
		if lim.MaxPeople > 0 && len(ps) >= lim.MaxPeople {
			return UpsertResult{}, fmt.Errorf(prompt.T(s.lang, "prompt.people.result.limit",
				"人物已达上限（%d 人），不再登记新人；可用 remove_person 移除不再往来的人，或在设置页调大上限。现有：%s"),
				lim.MaxPeople, nameList(s.lang, names(ps)))
		}
		p := Person{Name: name, Relation: trimmed(u.Relation), Closeness: defaultCloseness, Created: now, Updated: now}
		if u.Profile != nil {
			p.Profile = trimmed(u.Profile)
		}
		if u.Closeness != nil && *u.Closeness != "" {
			// 校验已在 validateFields 做过，这里取的是换算回中文的规范值
			p.Closeness, _ = closenessLevels.Canonical(*u.Closeness)
		}
		if u.MetNow {
			p.LastMet = now
			p.LastNote = trimmed(u.LastNote)
		}
		if l := trimmed(u.Lately); l != "" {
			p.Lately, p.LatelyAt = l, now
		}
		ps = append(ps, p)
		if err := s.save(ps); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{Name: name, Created: true}, nil
	}

	p := &ps[i]
	res := UpsertResult{Name: p.Name}
	if u.Relation != nil && trimmed(u.Relation) != p.Relation {
		if trimmed(u.Relation) == "" {
			return UpsertResult{}, fmt.Errorf("%s", prompt.T(s.lang, "prompt.people.result.relation_empty", "relation 不能清空"))
		}
		res.Changes = append(res.Changes, prompt.Tf(s.lang, "prompt.people.change.relation", "关系 %s→%s",
			p.Relation, trimmed(u.Relation)))
		p.Relation = trimmed(u.Relation)
	}
	if u.Profile != nil && trimmed(u.Profile) != p.Profile {
		p.Profile = trimmed(u.Profile)
		res.Changes = append(res.Changes, prompt.T(s.lang, "prompt.people.change.profile", "设定已更新"))
	}
	if u.Closeness != nil && *u.Closeness != "" {
		// 比的是换算回中文的规范值：英文界面下模型报 "close"，而库里存的是「熟」，
		// 直接比字符串会把「没变」当成一次变化，每轮都多出一句「亲近度 熟→close」。
		if want, _ := closenessLevels.Canonical(*u.Closeness); want != p.Closeness {
			res.Changes = append(res.Changes, prompt.Tf(s.lang, "prompt.people.change.closeness", "亲近度 %s→%s",
				closenessLevels.For(s.lang, p.Closeness), closenessLevels.For(s.lang, want)))
			p.Closeness = want
		}
	}
	if u.MetNow {
		p.LastMet = now
		if u.LastNote != nil {
			p.LastNote = trimmed(u.LastNote)
		}
		res.Changes = append(res.Changes, prompt.T(s.lang, "prompt.people.change.met_now", "上次互动记为此刻"))
	} else if u.LastNote != nil && trimmed(u.LastNote) != p.LastNote {
		p.LastNote = trimmed(u.LastNote)
		res.Changes = append(res.Changes, prompt.T(s.lang, "prompt.people.change.last_note", "上次互动的摘要已更新"))
	}
	if u.Lately != nil {
		// 传了就是「刚听说」：同一句话再传一次也刷新时刻，与关系的「最近」同一个口径
		switch l := trimmed(u.Lately); {
		case l == "" && p.Lately != "":
			p.Lately, p.LatelyAt = "", time.Time{}
			res.Changes = append(res.Changes, prompt.T(s.lang, "prompt.people.change.lately_cleared", "近况已清除"))
		case l != "":
			p.Lately, p.LatelyAt = l, now
			res.Changes = append(res.Changes, prompt.T(s.lang, "prompt.people.change.lately", "近况已更新"))
		}
	}
	if len(res.Changes) == 0 {
		return res, nil // 什么都没变就不写盘
	}
	p.Updated = now
	if err := s.save(ps); err != nil {
		return UpsertResult{}, err
	}
	return res, nil
}

// Remove 移除一个人物。
func (s *Store) Remove(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.load()
	if err != nil {
		return "", err
	}
	i := personIndex(ps, name)
	if i < 0 {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.people.result.no_such", "没有叫 %q 的人物，现有：%s"),
			strings.TrimSpace(name), nameList(s.lang, names(ps)))
	}
	removed := ps[i].Name
	ps = append(ps[:i], ps[i+1:]...)
	if err := s.save(ps); err != nil {
		return "", err
	}
	return removed, nil
}

// save 原子写回整个文件，避免进程中断留下半截内容。人物库属于对话内容的一部分，
// 目录与文件都收紧权限。
func (s *Store) save(ps []Person) error {
	if ps == nil {
		ps = []Person{}
	}
	raw, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.people.store.save_failed", "保存人物库失败: %w"), err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.people.store.mkdir_failed", "创建人物库目录失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.people.store.save_failed", "保存人物库失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.people.store.save_failed", "保存人物库失败: %w"), err)
	}
	return nil
}

// validateFields 校验各字段的长度与枚举值。
func validateFields(lang string, u Update) error {
	if err := checkRunes(lang, u.Relation, "relation", maxRelationRunes); err != nil {
		return err
	}
	if err := checkRunes(lang, u.Profile, "profile", maxProfileRunes); err != nil {
		return err
	}
	if err := checkRunes(lang, u.LastNote, "last_note", maxNoteRunes); err != nil {
		return err
	}
	if err := checkRunes(lang, u.Lately, "lately", maxLatelyRunes); err != nil {
		return err
	}
	if u.Closeness != nil && *u.Closeness != "" {
		if _, ok := closenessLevels.Canonical(*u.Closeness); !ok {
			return fmt.Errorf(prompt.T(lang, "prompt.people.result.bad_closeness", "closeness 只能是：%s"),
				strings.Join(closenessLevels.Values(lang), " / "))
		}
	}
	return nil
}

func checkRunes(lang string, v *string, what string, max int) error {
	if v == nil {
		return nil
	}
	if n := len([]rune(strings.TrimSpace(*v))); n > max {
		return fmt.Errorf(prompt.T(lang, "prompt.people.result.field_long",
			"%s过长（%d 字，上限 %d 字），请压缩后重试"), what, n, max)
	}
	return nil
}

// normalizeName 规整名字：压掉换行与连续空白，校验非空、长度与「不是对方本人」。
func normalizeName(lang, name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.people.result.name_empty", "name 不能为空"))
	}
	if n := len([]rune(name)); n > maxNameRunes {
		return "", fmt.Errorf(prompt.T(lang, "prompt.people.result.name_long",
			"名字过长（%d 字，上限 %d 字），请用更短的称呼"), n, maxNameRunes)
	}
	if _, isSelf := selfNames.Canonical(name); isSelf {
		return "", fmt.Errorf(prompt.T(lang, "prompt.people.result.is_self",
			"%q 指的是对方本人，不登记在人物库里；对方的情况按 [对方信息] 与记忆处理"), name)
	}
	return name, nil
}

func trimmed(v *string) string {
	if v == nil {
		return ""
	}
	return strings.Join(strings.Fields(*v), " ")
}

// personIndex 按名字查找（大小写不敏感）。
func personIndex(ps []Person, name string) int {
	want := strings.ToLower(strings.Join(strings.Fields(name), " "))
	for i, p := range ps {
		if strings.ToLower(p.Name) == want {
			return i
		}
	}
	return -1
}

func names(ps []Person) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// nameList 把候选名拼成一句，限量。
func nameList(lang string, ns []string) string {
	const maxNames = 20
	if len(ns) == 0 {
		return prompt.T(lang, "prompt.people.render.empty", "（空）")
	}
	shown := ns
	var more string
	if len(shown) > maxNames {
		more = prompt.Tf(lang, "prompt.people.render.and_more", " 等 %d 人", len(ns))
		shown = shown[:maxNames]
	}
	return strings.Join(shown, prompt.T(lang, "prompt.people.render.sep", "、")) + more
}
