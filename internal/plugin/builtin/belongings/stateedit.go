package belongings

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// EditState 处理状态页上的持有物修正：放入与取出。
//
// 域名、容器/物品身份与操作都来自客户端：先按磁盘上真实存在的域核对，再交给
// Store.Apply——同名叠加、数量与条数上限都在那边。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	s := p.snapshot()
	if s.base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.belongings.state.err.not_ready", "持有物清单尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(plugin.ReadDomains(s.base, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.belongings.state.err.unknown_target", "没有要修改的这一块。"))
	}
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.belongings.state.err.not_ready", "持有物清单尚未就绪。"))
	}

	switch req.Op {
	case stateOpTake:
		return p.takeItem(req, store, s)
	case stateOpAdd:
		return p.addItemFromState(req, store, s)
	}
	return errors.New(i18n.T(req.Lang, "plugin.belongings.state.err.unknown_op", "不认识的修改操作。"))
}

func (p *Plugin) takeItem(req plugin.StateEditRequest, store *Store, s settings) error {
	container, item, ok := splitItemID(req.Row)
	if !ok {
		return errors.New(i18n.T(req.Lang, "plugin.belongings.state.err.no_such", "这件东西已经不在了。"))
	}
	qty, err := parseQty(req.Lang, req.Values["qty"])
	if err != nil {
		return err
	}
	cs, err := store.List()
	if err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	ci := containerIndex(cs, container)
	if ci < 0 {
		return errors.New(i18n.T(req.Lang, "plugin.belongings.state.err.no_such", "这件东西已经不在了。"))
	}
	ii := itemIndex(cs[ci].Items, item)
	if ii < 0 {
		return errors.New(i18n.T(req.Lang, "plugin.belongings.state.err.no_such", "这件东西已经不在了。"))
	}
	if _, err := store.Apply(cs[ci].Name, nil,
		[]Change{{Name: cs[ci].Items[ii].Name, Qty: qty}}, time.Now(),
		Limits{MaxContainers: s.maxContainers, MaxItemsPerContainer: s.maxItems}); err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	return nil
}

func (p *Plugin) addItemFromState(req plugin.StateEditRequest, store *Store, s settings) error {
	container := collapseSpace(req.Values["container"])
	name := collapseSpace(req.Values["name"])
	note := strings.TrimSpace(req.Values["note"])
	if container == "" {
		return errors.New(i18n.T(req.Lang, "plugin.belongings.state.err.container_required", "容器名不能为空。"))
	}
	if err := checkLen(req.Lang, container, maxContainerNameRunes); err != nil {
		return err
	}
	if name == "" {
		return errors.New(i18n.T(req.Lang, "plugin.belongings.state.err.item_required", "物品名不能为空。"))
	}
	if err := checkLen(req.Lang, name, maxItemNameRunes); err != nil {
		return err
	}
	if n := len([]rune(note)); n > maxNoteRunes {
		return errors.New(i18n.Tf(req.Lang, "plugin.belongings.state.err.too_long",
			"内容太长（%d 字，上限 %d 字）。", n, maxNoteRunes))
	}
	qty, err := parseQty(req.Lang, req.Values["qty"])
	if err != nil {
		return err
	}
	if _, err := store.Apply(container,
		[]Change{{Name: name, Qty: qty, Note: note}}, nil, time.Now(),
		Limits{MaxContainers: s.maxContainers, MaxItemsPerContainer: s.maxItems}); err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	return nil
}

func parseQty(lang, raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > maxQty {
		return 0, errors.New(i18n.T(lang, "plugin.belongings.state.err.qty_range", "数量要在 0~9999 之间（0 表示不计数）。"))
	}
	return n, nil
}

// checkLen 校验名称长度。空名的报错在调用处各写一句，键因此能写成字面量。
func checkLen(lang, v string, max int) error {
	if n := len([]rune(v)); n > max {
		return errors.New(i18n.Tf(lang, "plugin.belongings.state.err.too_long",
			"名称太长（%d 字，上限 %d 字）。", n, max))
	}
	return nil
}

func (p *Plugin) stateEditFailed(lang string, err error) error {
	i18n.Logf(p.langOf(), "log.belongings.state_edit_failed", "状态页修改持有物失败：%v", err)
	return errors.New(i18n.T(lang, "plugin.belongings.state.err.save_failed", "保存持有物失败，请稍后重试。"))
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

var _ plugin.StateEditor = (*Plugin)(nil)
