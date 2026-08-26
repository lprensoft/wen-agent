// Package belongings 提供持有物清单的系统插件：把冰箱、衣柜这类容器里的物品记成
// 可增减的清单，随演绎更新（买菜入库、做菜消耗、衣服穿坏丢弃），跨会话与压缩保持，
// 并在每轮注入当前持有情况供演绎使用。
//
// 本插件依赖 roleplay：清单描述的是角色的持有物，没有角色就没有归属对象。
//
// 与相邻插件的分工：presence 记「此刻穿着什么、拿着什么」（现场），memory 记
// 「物品的来历与意义」（事件），本插件只记「现在有什么、放了多久」（账本）。
package belongings

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
	// maxContainerNameRunes / maxItemNameRunes / maxNoteRunes 限制单条内容的体量：
	// 清单会随每轮对话全文注入，单条必须有确定上界。超长时报错让模型压缩后重试，
	// 而不是悄悄截断——被截掉的部分模型不知道丢了。
	maxContainerNameRunes = 20
	maxItemNameRunes      = 30
	maxNoteRunes          = 80
	// maxQty 限制单个物品的数量：清单记的是身边的持有物，四位数已经不是「清单」
	// 该管的粒度了。
	maxQty = 9999
)

// Item 是容器里的一件（或一批）物品。
type Item struct {
	Name string `json:"name"`
	// Qty 为 0 表示不计数（一件外套），大于 0 表示可数的数量（鸡蛋×6）。
	Qty  int    `json:"qty,omitempty"`
	Note string `json:"note,omitempty"`
	// Added 是入库时刻，注入时据此渲染「N 天前放入」，给模型判断新鲜与新旧的线索。
	// 同名叠加时更新为最新一次。
	Added   time.Time `json:"added"`
	Updated time.Time `json:"updated"`
}

// Container 是一个容器（冰箱、衣柜……）与其中的物品，条目保持记录顺序。
type Container struct {
	Name    string    `json:"name"`
	Items   []Item    `json:"items"`
	Updated time.Time `json:"updated"`
	// Domain 是这个容器所属的可见域标签，不落盘：它由所在的库决定，
	// 跨库合并后用于把后续写操作送回正确的库。
	Domain string `json:"-"`
}

// Change 是 update_items 里的一项变化：add 时 Qty 是放入的数量（0=不计数），
// remove 时 Qty 是取出的数量（0=整条移除）。
type Change struct {
	Name string
	Qty  int
	Note string
}

// Limits 是写入时生效的上限，来自插件配置。
type Limits struct {
	MaxContainers        int
	MaxItemsPerContainer int
}

// ApplyResult 是一次更新的回执素材，片段已渲染好（「鸡蛋×6」「牛奶（余 1）」）。
type ApplyResult struct {
	Container string
	Added     []string
	Removed   []string
}

// Store 管理一个可见域的清单库：单个 JSON 文件。文件可能被用户在进程外修改，
// 因此每次操作都重新读取，不做缓存——条目数量有上限，读取开销可以忽略。
type Store struct {
	mu sync.Mutex
	// lang 是进程语言。这个库的错误会经工具返回给模型，因此要跟着语言走。
	lang string
	path string
}

// NewStore 建立指向 <dir>/containers.json 的清单库。
func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, path: filepath.Join(dir, "containers.json")}
}

// List 返回全部容器，按记录顺序。文件不存在时返回空列表。
func (s *Store) List() ([]Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() ([]Container, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.belongings.store.read_failed_err", "读取持有物清单失败: %w"), err)
	}
	var cs []Container
	if err := json.Unmarshal(raw, &cs); err != nil {
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.belongings.store.corrupt", "持有物清单文件损坏: %w"), err)
	}
	return cs, nil
}

// Apply 对一个容器做一批增减：先处理 remove（腾出位置），再处理 add。
// 整批在同一临界区内完成并一次写回，任何一项校验失败都整批不落盘——
// 半批生效会让回执与磁盘状态对不上。
func (s *Store) Apply(container string, add, remove []Change, now time.Time, lim Limits) (ApplyResult, error) {
	cname, err := normalizeName(s.lang, container, containerWhat, maxContainerNameRunes)
	if err != nil {
		return ApplyResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cs, err := s.load()
	if err != nil {
		return ApplyResult{}, err
	}

	ci := containerIndex(cs, cname)
	if ci < 0 {
		if len(add) == 0 {
			return ApplyResult{}, fmt.Errorf(prompt.T(s.lang, "prompt.belongings.result.no_container",
				"没有名为 %q 的容器，现有：%s"), cname, nameList(s.lang, containerNames(cs)))
		}
		// 拦截生效时要把规则告诉模型，否则它只会换个名字再试一次
		if lim.MaxContainers > 0 && len(cs) >= lim.MaxContainers {
			return ApplyResult{}, fmt.Errorf(prompt.T(s.lang, "prompt.belongings.result.container_limit",
				"容器已达上限（%d 个），不再新建；可把物品记进已有容器，或在设置页调大上限。现有：%s"),
				lim.MaxContainers, nameList(s.lang, containerNames(cs)))
		}
		cs = append(cs, Container{Name: cname})
		ci = len(cs) - 1
	}
	cont := &cs[ci]
	res := ApplyResult{Container: cont.Name}

	for _, ch := range remove {
		frag, err := removeItem(s.lang, cont, ch, now)
		if err != nil {
			return ApplyResult{}, err
		}
		res.Removed = append(res.Removed, frag)
	}
	for _, ch := range add {
		frag, err := addItem(s.lang, cont, ch, now, lim)
		if err != nil {
			return ApplyResult{}, err
		}
		res.Added = append(res.Added, frag)
	}

	cont.Updated = now
	if err := s.save(cs); err != nil {
		return ApplyResult{}, err
	}
	return res, nil
}

// removeItem 从容器里取出/消耗一项。Qty 为 0、物品不计数、或减到 0 时整条移除。
func removeItem(lang string, cont *Container, ch Change, now time.Time) (string, error) {
	name := strings.TrimSpace(ch.Name)
	i := itemIndex(cont.Items, name)
	if i < 0 {
		return "", fmt.Errorf(prompt.T(lang, "prompt.belongings.result.no_item",
			"%s里没有 %q，现有：%s"), cont.Name, name, nameList(lang, itemNames(cont.Items)))
	}
	if ch.Qty < 0 {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.belongings.result.negative_qty", "取出数量不能为负"))
	}
	it := &cont.Items[i]
	if ch.Qty > 0 && it.Qty > ch.Qty {
		it.Qty -= ch.Qty
		it.Updated = now
		return prompt.Tf(lang, "prompt.belongings.result.taken_left",
			"%s×%d（余 %d）", it.Name, ch.Qty, it.Qty), nil
	}
	frag := it.Name
	if it.Qty > 0 {
		frag = prompt.Tf(lang, "prompt.belongings.result.taken_all", "%s×%d（已取完）", it.Name, it.Qty)
	}
	cont.Items = append(cont.Items[:i], cont.Items[i+1:]...)
	return frag, nil
}

// addItem 往容器里放入一项。同名已存在时叠加数量、更新备注，并把入库时刻
// 更新为最新一次——分不清新旧批次时，按最近的算对「还新鲜吗」最不误事。
func addItem(lang string, cont *Container, ch Change, now time.Time, lim Limits) (string, error) {
	name, err := normalizeName(lang, ch.Name, itemWhat, maxItemNameRunes)
	if err != nil {
		return "", err
	}
	if ch.Qty < 0 || ch.Qty > maxQty {
		return "", fmt.Errorf(prompt.T(lang, "prompt.belongings.result.qty_range",
			"数量要在 0~%d 之间（0 表示不计数）"), maxQty)
	}
	note := strings.TrimSpace(ch.Note)
	if n := len([]rune(note)); n > maxNoteRunes {
		return "", fmt.Errorf(prompt.T(lang, "prompt.belongings.result.note_long",
			"备注过长（%d 字，上限 %d 字），请压缩后重试"), n, maxNoteRunes)
	}

	if i := itemIndex(cont.Items, name); i >= 0 {
		it := &cont.Items[i]
		if ch.Qty > 0 {
			if it.Qty+ch.Qty > maxQty {
				return "", fmt.Errorf(prompt.T(lang, "prompt.belongings.result.qty_overflow",
					"%q 叠加后数量超出上限 %d"), it.Name, maxQty)
			}
			it.Qty += ch.Qty
		}
		if note != "" {
			it.Note = note
		}
		it.Added = now
		it.Updated = now
		if it.Qty > 0 {
			return prompt.Tf(lang, "prompt.belongings.result.added_total",
				"%s×%d（现共 %d）", it.Name, ch.Qty, it.Qty), nil
		}
		return it.Name + prompt.T(lang, "prompt.belongings.result.updated", "（已有，更新）"), nil
	}

	// 拦截生效时要把规则告诉模型
	if lim.MaxItemsPerContainer > 0 && len(cont.Items) >= lim.MaxItemsPerContainer {
		return "", fmt.Errorf(prompt.T(lang, "prompt.belongings.result.item_limit",
			"%s的物品已达上限（%d 项），先取出不再需要的，或在设置页调大上限"),
			cont.Name, lim.MaxItemsPerContainer)
	}
	cont.Items = append(cont.Items, Item{Name: name, Qty: ch.Qty, Note: note, Added: now, Updated: now})
	if ch.Qty > 0 {
		return fmt.Sprintf("%s×%d", name, ch.Qty), nil
	}
	return name, nil
}

// save 原子写回整个文件，避免进程中断留下半截内容。清单属于对话内容的一部分，
// 目录与文件都收紧权限。
func (s *Store) save(cs []Container) error {
	if cs == nil {
		cs = []Container{}
	}
	raw, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.belongings.store.save_failed", "保存持有物清单失败: %w"), err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.belongings.store.mkdir_failed", "创建持有物清单目录失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.belongings.store.save_failed", "保存持有物清单失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.belongings.store.save_failed", "保存持有物清单失败: %w"), err)
	}
	return nil
}

// containerIndex / itemIndex 按名称查找（大小写不敏感）。
func containerIndex(cs []Container, name string) int {
	want := strings.ToLower(strings.TrimSpace(name))
	for i, c := range cs {
		if strings.ToLower(c.Name) == want {
			return i
		}
	}
	return -1
}

func itemIndex(items []Item, name string) int {
	want := strings.ToLower(strings.TrimSpace(name))
	for i, it := range items {
		if strings.ToLower(it.Name) == want {
			return i
		}
	}
	return -1
}

func containerNames(cs []Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func itemNames(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

// nameList 把候选名拼成一句，限量——装了很多东西时，一次拼错不该灌进去一大段。
func nameList(lang string, names []string) string {
	const maxNames = 20
	if len(names) == 0 {
		return prompt.T(lang, "prompt.belongings.render.empty", "（空）")
	}
	shown := names
	var more string
	if len(shown) > maxNames {
		more = prompt.Tf(lang, "prompt.belongings.render.and_more", " 等 %d 项", len(names))
		shown = shown[:maxNames]
	}
	return strings.Join(shown, prompt.T(lang, "prompt.belongings.render.sep", "、")) + more
}

// normalizeName 规整名称：压掉换行与连续空白，校验非空与长度。
func normalizeName(lang, name string, what prompt.Piece, maxRunes int) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", fmt.Errorf(prompt.T(lang, "prompt.belongings.result.name_empty", "%s不能为空"), what.For(lang))
	}
	if n := len([]rune(name)); n > maxRunes {
		return "", fmt.Errorf(prompt.T(lang, "prompt.belongings.result.name_long",
			"%s过长（%d 字，上限 %d 字），请用更短的名称"), what.For(lang), n, maxRunes)
	}
	return name, nil
}

// containerWhat / itemWhat 是上面两句报错里的「哪一种名称」。
var (
	containerWhat = prompt.Text("prompt.belongings.what.container", "容器名称")
	itemWhat      = prompt.Text("prompt.belongings.what.item", "物品名称")
)
