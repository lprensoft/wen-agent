package scene

import (
	"errors"
	"slices"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// EditState 处理状态页上的场景修正。
//
// 域名、行名与操作都来自客户端：先按磁盘上真实存在的域核对，再交给 Store 的
// Save / Delete——校验与原子写都在那边。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	s := p.snapshot()
	if s.base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.scene.state.err.not_ready", "场景记录尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(plugin.ReadDomains(s.base, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.scene.state.err.unknown_target", "没有要修改的这一块。"))
	}
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.scene.state.err.not_ready", "场景记录尚未就绪。"))
	}

	switch req.Op {
	case stateOpAdd:
		return p.addScene(req, store)
	case stateOpEdit:
		return p.editScene(req, store)
	case stateOpDelete:
		return p.deleteScene(req, store)
	}
	return errors.New(i18n.T(req.Lang, "plugin.scene.state.err.unknown_op", "不认识的修改操作。"))
}

func (p *Plugin) addScene(req plugin.StateEditRequest, store *Store) error {
	name := collapseSpace(req.Values["name"])
	detail := strings.TrimSpace(req.Values["detail"])
	if err := checkSceneName(req.Lang, name); err != nil {
		return err
	}
	if err := checkDetail(req.Lang, detail); err != nil {
		return err
	}
	scenes, err := store.List()
	if err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	if indexOf(scenes, name) >= 0 {
		return errors.New(i18n.Tf(req.Lang, "plugin.scene.state.err.duplicate", "已经记录过 %q 了。", name))
	}
	if _, err := store.SaveAt(Scene{Name: name, Detail: detail}, false, time.Now()); err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	return nil
}

func (p *Plugin) editScene(req plugin.StateEditRequest, store *Store) error {
	if !req.Touched("detail") {
		return nil
	}
	scenes, err := store.List()
	if err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	i := indexOf(scenes, req.Row)
	if i < 0 {
		return errors.New(i18n.T(req.Lang, "plugin.scene.state.err.no_such", "这处场景已经不在了。"))
	}
	detail := strings.TrimSpace(req.Values["detail"])
	if err := checkDetail(req.Lang, detail); err != nil {
		return err
	}
	if _, err := store.SaveAt(Scene{Name: scenes[i].Name, Detail: detail}, true, time.Now()); err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	return nil
}

func (p *Plugin) deleteScene(req plugin.StateEditRequest, store *Store) error {
	scenes, err := store.List()
	if err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	i := indexOf(scenes, req.Row)
	if i < 0 {
		return errors.New(i18n.T(req.Lang, "plugin.scene.state.err.no_such", "这处场景已经不在了。"))
	}
	if _, err := store.Delete(scenes[i].Name); err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	return nil
}

func (p *Plugin) stateEditFailed(lang string, err error) error {
	i18n.Logf(p.snapshot().lang, "log.scene.state_edit_failed", "状态页修改场景失败：%v", err)
	return errors.New(i18n.T(lang, "plugin.scene.state.err.save_failed", "保存场景失败，请稍后重试。"))
}

func checkSceneName(lang, name string) error {
	switch {
	case name == "":
		return errors.New(i18n.T(lang, "plugin.scene.state.err.name_required", "名称不能为空。"))
	case len([]rune(name)) > maxNameRunes:
		return errors.New(i18n.Tf(lang, "plugin.scene.state.err.too_long",
			"名称太长（%d 字，上限 %d 字）。", len([]rune(name)), maxNameRunes))
	}
	return nil
}

func checkDetail(lang, detail string) error {
	if detail == "" {
		return errors.New(i18n.T(lang, "plugin.scene.state.err.detail_required", "描述不能为空。"))
	}
	if n := len([]rune(detail)); n > maxDetailRunes {
		return errors.New(i18n.Tf(lang, "plugin.scene.state.err.too_long",
			"描述太长（%d 字，上限 %d 字）。", n, maxDetailRunes))
	}
	return nil
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

var _ plugin.StateEditor = (*Plugin)(nil)
