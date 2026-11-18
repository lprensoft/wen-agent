package belongings

import (
	"fmt"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stateMaxRows 是状态页上最多列几行（容器与物品合计）。账本本身有配置上限，
// 这里只是兜住页面长度。
const stateMaxRows = 40

// State 把各可见域的持有物交给设置页的「角色状态」。
//
// 每件物品一行，容器名放在行首的 Meta 里；空容器也留一行，否则主人看不到
// 那处清单存在、也就不知道该往哪儿添东西。按域分块，其余人格域由界面折叠。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	s := p.snapshot()
	if s.base == "" {
		return nil
	}

	var out []plugin.StateSection
	failed := false
	for _, tag := range plugin.ReadDomains(s.base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		cs, err := store.List()
		if err != nil {
			i18n.Logf(p.langOf(), "log.belongings.state_failed", "读取持有物状态失败（域 %q）: %v", tag, err)
			failed = true
			continue
		}
		if len(cs) == 0 {
			continue
		}

		rows := make([]plugin.StateRow, 0, stateMaxRows)
		var latest time.Time
		items := 0
		for _, c := range cs {
			if c.Updated.After(latest) {
				latest = c.Updated
			}
			if len(c.Items) == 0 {
				if len(rows) < stateMaxRows {
					rows = append(rows, plugin.StateRow{
						Meta:  c.Name,
						Value: i18n.T(q.Lang, "plugin.belongings.state.empty_container", "（空）"),
						Tone:  plugin.StateToneMuted,
					})
				}
				continue
			}
			for _, it := range c.Items {
				items++
				if len(rows) >= stateMaxRows {
					continue
				}
				value := it.Name
				if it.Qty > 0 {
					value += fmt.Sprintf("×%d", it.Qty)
				}
				rows = append(rows, plugin.StateRow{
					Meta:   c.Name,
					Value:  value,
					Detail: itemDetail(q.Lang, it),
					ID:     itemID(c.Name, it.Name),
					Edits:  itemEdits(q.Lang, it),
				})
			}
		}
		out = append(out, plugin.StateSection{
			Key:   "belongings",
			Title: i18n.T(q.Lang, "plugin.belongings.state.title", "持有物"),
			Summary: i18n.Tf(q.Lang, "plugin.belongings.state.summary", "%d 个容器 · %d 件物品",
				len(cs), items),
			Rows:      rows,
			Domain:    tag,
			UpdatedAt: latest,
			More:      rowCount(cs) - len(rows),
			Edits:     addEdits(q.Lang),
		})
	}
	if len(out) == 0 {
		if failed {
			return nil
		}
		return []plugin.StateSection{{
			Key:   "belongings",
			Title: i18n.T(q.Lang, "plugin.belongings.state.title", "持有物"),
			Empty: i18n.T(q.Lang, "plugin.belongings.state.empty", "还没有记录持有物。"),
			Edits: addEdits(q.Lang),
		}}
	}
	return out
}

// 状态页上的操作标识。静态、不翻译。
const (
	stateSection = "belongings"
	stateOpTake  = "take"
	stateOpAdd   = "add"
)

// itemID 把「哪个容器的哪一件」拼成一个不透明身份。用单元分隔符而不是斜杠：
// 容器名与物品名都可能带普通标点，分隔符不该和它们撞上。
func itemID(container, item string) string { return container + "\x1f" + item }

func splitItemID(id string) (container, item string, ok bool) {
	return strings.Cut(id, "\x1f")
}

func itemEdits(lang string, it Item) []plugin.StateEdit {
	return []plugin.StateEdit{{
		Op:    stateOpTake,
		Label: i18n.T(lang, "plugin.belongings.state.take", "取出"),
		Fields: []plugin.StateField{{
			Key: "qty", Label: i18n.T(lang, "plugin.belongings.state.take_qty", "取出数量（0 = 全部）"),
			Type: plugin.StateFieldNumber, Value: "0",
			Min: plugin.IntPtr(0), Max: plugin.IntPtr(maxQty),
		}},
	}}
}

func addEdits(lang string) []plugin.StateEdit {
	return []plugin.StateEdit{{
		Op:    stateOpAdd,
		Label: i18n.T(lang, "plugin.belongings.state.add", "放入"),
		Fields: []plugin.StateField{
			{
				Key: "container", Label: i18n.T(lang, "plugin.belongings.state.field.container", "容器"),
				Type: plugin.StateFieldText, MaxLen: maxContainerNameRunes, Required: true,
			},
			{
				Key: "name", Label: i18n.T(lang, "plugin.belongings.state.field.item", "物品"),
				Type: plugin.StateFieldText, MaxLen: maxItemNameRunes, Required: true,
			},
			{
				Key: "qty", Label: i18n.T(lang, "plugin.belongings.state.field.qty", "数量（0 = 不计数）"),
				Type: plugin.StateFieldNumber, Value: "0",
				Min: plugin.IntPtr(0), Max: plugin.IntPtr(maxQty),
			},
			{
				Key: "note", Label: i18n.T(lang, "plugin.belongings.state.field.note", "备注"),
				Type: plugin.StateFieldText, MaxLen: maxNoteRunes,
			},
		},
	}}
}

// itemDetail 拼一行备注与入库日期。日期用中性的 ISO 写法，不另造相对时间的说法。
func itemDetail(lang string, it Item) string {
	detail := it.Note
	if !it.Added.IsZero() {
		if detail != "" {
			detail += " · "
		}
		detail += i18n.Tf(lang, "plugin.belongings.state.added", "放入 %s", it.Added.Format("2006-01-02"))
	}
	return detail
}

// rowCount 是这块一共会占几行：每件物品一行，空容器各占一行。
func rowCount(cs []Container) int {
	n := 0
	for _, c := range cs {
		if len(c.Items) == 0 {
			n++
			continue
		}
		n += len(c.Items)
	}
	return n
}
