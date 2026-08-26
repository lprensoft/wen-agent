// Package relationship 提供关系状态的系统插件：角色与对方之间「处在什么阶段、彼此怎么
// 称呼、最近一次关系上的变动、相处出来的默契与禁区、对方的近况」记成一份小快照，由
// 模型在关系真的变了时更新，每轮注入上下文。
//
// 它解决的是态度漂移：性格稳定有一半是「对这个人的态度稳定」，而此前角色对对方的态度
// 每轮都从接触记录与记忆索引里重新推一遍，于是忽远忽近。快照落盘在会话之外，天然跨
// 会话、跨压缩接力。
//
// 本插件依赖 roleplay：关系是角色与对方之间的，没有角色就没有归属对象。对方本人的事实
// 不记在这里——那由 roleplay 的「我的信息」与记忆负责，这里只记「我们之间」。
package relationship

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

// 各字段的长度上限。快照随每轮对话注入，单项必须有确定上界。超长时报错让模型压缩后
// 重试，而不是悄悄截断——一条被截掉一半的禁区，意思可能正好反过来。
const (
	maxStageRunes    = 10
	maxCallRunes     = 10
	maxRecentRunes   = 60
	maxBonds         = 5
	maxBondRunes     = 30
	maxTheirNowRunes = 40

	// recentTTL 是「最近一次变动」的有效期：过了这么久还挂着的「最近」已经不是最近，
	// 注入时不再出现（落盘的内容保留，模型下次更新会覆盖）。
	recentTTL = 30 * 24 * time.Hour
)

// Snapshot 是关系状态的一份快照。字段全部可选。
type Snapshot struct {
	Stage     string `json:"stage,omitempty"`
	MyCall    string `json:"my_call,omitempty"`
	TheirCall string `json:"their_call,omitempty"`
	// Recent 是最近一次关系上的变动，RecentAt 记它写下的时刻，注入时据此标「N 天前」。
	Recent   string    `json:"recent,omitempty"`
	RecentAt time.Time `json:"recent_at,omitempty"`
	Bonds    []string  `json:"bonds,omitempty"`
	TheirNow string    `json:"their_now,omitempty"`
	Updated  time.Time `json:"updated"`
}

// Empty 判断快照是否一项内容都没有。
func (s Snapshot) Empty() bool {
	return s.Stage == "" && s.MyCall == "" && s.TheirCall == "" &&
		s.Recent == "" && len(s.Bonds) == 0 && s.TheirNow == ""
}

// Update 是 update_relationship 的一次改动。指针为 nil 表示「这次没传」，与传空串
// 区分开：模型只传要改的字段。Bonds 整体覆盖，传空切片表示清空。
type Update struct {
	Stage     *string
	MyCall    *string
	TheirCall *string
	Recent    *string
	Bonds     *[]string
	TheirNow  *string
}

// 回显用的字段标签。摆成表再按情况取用，故用 Piece 把键与原文绑在声明处。
var (
	labelStage     = prompt.Text("prompt.relationship.label.stage", "阶段")
	labelMyCall    = prompt.Text("prompt.relationship.label.my_call", "你对对方的称呼")
	labelTheirCall = prompt.Text("prompt.relationship.label.their_call", "对方对你的称呼")
	labelRecent    = prompt.Text("prompt.relationship.label.recent", "最近变动")
	labelBonds     = prompt.Text("prompt.relationship.label.bonds", "默契与禁区")
	labelTheirNow  = prompt.Text("prompt.relationship.label.their_now", "对方近况")
)

// Store 管理一个可见域的关系快照。单个 JSON 文件，每次操作重新读取不做缓存。
type Store struct {
	mu   sync.Mutex
	lang string
	dir  string
	path string
}

// NewStore 建立指向 <dir>/relationship.json 的快照库。lang 决定回显标签与报错的语言。
func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, dir: dir, path: filepath.Join(dir, "relationship.json")}
}

// Load 返回当前快照。第二个返回值表示是否有过记录。
func (s *Store) Load() (Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Apply 应用一批字段变更：非空值更新该字段，空串清除该字段。整批校验通过才落盘。
// 返回实际更新与清除的字段标签，供工具回显。
//
// recent 只要传了非空值就把时刻记为此刻，文字与上次相同也一样：模型只传变化的字段，
// 再传一次同样的话就是「又发生了一次」，而不是「没变」。
func (s *Store) Apply(u Update, now time.Time) (updated, cleared []string, err error) {
	if err := validate(s.lang, u); err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	snap, _, err := s.load()
	if err != nil {
		return nil, nil, err
	}

	setText := func(field *string, v *string, label string) {
		if v == nil {
			return
		}
		text := normalize(*v)
		switch {
		case text == "" && *field != "":
			*field = ""
			cleared = append(cleared, label)
		case text != "" && text != *field:
			*field = text
			updated = append(updated, label)
		}
	}
	setText(&snap.Stage, u.Stage, labelStage.For(s.lang))
	setText(&snap.MyCall, u.MyCall, labelMyCall.For(s.lang))
	setText(&snap.TheirCall, u.TheirCall, labelTheirCall.For(s.lang))
	if u.Recent != nil {
		text := normalize(*u.Recent)
		if text == "" {
			if snap.Recent != "" {
				snap.Recent, snap.RecentAt = "", time.Time{}
				cleared = append(cleared, labelRecent.For(s.lang))
			}
		} else {
			snap.Recent, snap.RecentAt = text, now
			updated = append(updated, labelRecent.For(s.lang))
		}
	}
	if u.Bonds != nil {
		bonds := normalizeBonds(*u.Bonds)
		switch {
		case len(bonds) == 0 && len(snap.Bonds) > 0:
			snap.Bonds = nil
			cleared = append(cleared, labelBonds.For(s.lang))
		case len(bonds) > 0 && strings.Join(bonds, "\x00") != strings.Join(snap.Bonds, "\x00"):
			snap.Bonds = bonds
			updated = append(updated, labelBonds.For(s.lang))
		}
	}
	setText(&snap.TheirNow, u.TheirNow, labelTheirNow.For(s.lang))

	if len(updated) == 0 && len(cleared) == 0 {
		return nil, nil, nil // 没有实际变化，不写盘
	}
	if snap.Empty() {
		// 全部字段都清掉了：删文件而不是留一个空对象
		if err := s.remove(); err != nil {
			return nil, nil, err
		}
		return updated, cleared, nil
	}
	snap.Updated = now
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
		return fmt.Errorf(prompt.T(s.lang, "prompt.relationship.err.clear", "清除关系状态失败: %w"), err)
	}
	return nil
}

func (s *Store) load() (Snapshot, bool, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Snapshot{}, false, nil
		}
		return Snapshot{}, false, fmt.Errorf(prompt.T(s.lang, "prompt.relationship.err.read", "读取关系状态失败: %w"), err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, false, fmt.Errorf(prompt.T(s.lang, "prompt.relationship.err.corrupt", "关系状态文件损坏: %w"), err)
	}
	// 手改过的文件不该把长度上限撑破
	snap.Stage = clip(snap.Stage, maxStageRunes)
	snap.MyCall = clip(snap.MyCall, maxCallRunes)
	snap.TheirCall = clip(snap.TheirCall, maxCallRunes)
	snap.Recent = clip(snap.Recent, maxRecentRunes)
	snap.TheirNow = clip(snap.TheirNow, maxTheirNowRunes)
	bonds := normalizeBonds(snap.Bonds)
	if len(bonds) > maxBonds {
		bonds = bonds[:maxBonds]
	}
	for i, b := range bonds {
		bonds[i] = clip(b, maxBondRunes)
	}
	snap.Bonds = bonds
	if snap.Recent == "" {
		snap.RecentAt = time.Time{}
	}
	return snap, !snap.Empty(), nil
}

// save 原子写回，权限 0600：关系状态属于对话内容的一部分。
func (s *Store) save(snap Snapshot) error {
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.relationship.err.save", "保存关系状态失败: %w"), err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.relationship.err.mkdir", "创建关系状态目录失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.relationship.err.save", "保存关系状态失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.relationship.err.save", "保存关系状态失败: %w"), err)
	}
	return nil
}

// validate 校验各字段的长度与条数。
func validate(lang string, u Update) error {
	if err := checkRunes(lang, u.Stage, "stage", maxStageRunes); err != nil {
		return err
	}
	if err := checkRunes(lang, u.MyCall, "my_call", maxCallRunes); err != nil {
		return err
	}
	if err := checkRunes(lang, u.TheirCall, "their_call", maxCallRunes); err != nil {
		return err
	}
	if err := checkRunes(lang, u.Recent, "recent", maxRecentRunes); err != nil {
		return err
	}
	if err := checkRunes(lang, u.TheirNow, "their_now", maxTheirNowRunes); err != nil {
		return err
	}
	if u.Bonds != nil {
		bonds := normalizeBonds(*u.Bonds)
		if len(bonds) > maxBonds {
			return fmt.Errorf(prompt.T(lang, "prompt.relationship.err.bonds_many",
				"bonds 过多（%d 条，上限 %d 条），请只留最要紧的几条"), len(bonds), maxBonds)
		}
		for _, b := range bonds {
			if n := len([]rune(b)); n > maxBondRunes {
				return fmt.Errorf(prompt.T(lang, "prompt.relationship.err.bond_long",
					"bonds 里有一条过长（%d 字，上限 %d 字）：%q，请压缩后重试"), n, maxBondRunes, b)
			}
		}
	}
	return nil
}

// checkRunes 的 what 是 JSON 字段名，不翻译——它是标识符，模型要照它改参数。
func checkRunes(lang string, v *string, what string, max int) error {
	if v == nil {
		return nil
	}
	if n := len([]rune(normalize(*v))); n > max {
		return fmt.Errorf(prompt.T(lang, "prompt.relationship.err.field_long",
			"%s 过长（%d 字，上限 %d 字），请压缩后重试"), what, n, max)
	}
	return nil
}

// normalize 压掉换行与连续空白。
func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// normalizeBonds 规整每一条并丢掉空条目。
func normalizeBonds(in []string) []string {
	var out []string
	for _, b := range in {
		if b = normalize(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// clip 规整并截断，只用于读入手改过的文件。
func clip(s string, maxRunes int) string {
	s = normalize(s)
	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes])
	}
	return s
}
