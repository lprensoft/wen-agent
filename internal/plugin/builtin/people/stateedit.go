package people

import (
	"errors"
	"slices"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// EditState 处理状态页上的人物修正。
//
// 域名、行名、操作都来自客户端，先按磁盘上真实存在的域与名册核对，再交给 Store
// 的写入接口——校验、原子写与可见域目录都由那边负责，这里不直接碰文件。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	s := p.snapshot()
	if s.base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.people.state.err.not_ready", "人物库尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(plugin.ReadDomains(s.base, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.people.state.err.unknown_target", "没有要修改的这一块。"))
	}
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.people.state.err.not_ready", "人物库尚未就绪。"))
	}

	switch req.Op {
	case stateOpAdd:
		return p.addPerson(req, store, s)
	case stateOpEdit:
		return p.editPerson(req, store)
	case stateOpDelete:
		return p.deletePerson(req, store)
	}
	return errors.New(i18n.T(req.Lang, "plugin.people.state.err.unknown_op", "不认识的修改操作。"))
}

func (p *Plugin) addPerson(req plugin.StateEditRequest, store *Store, s settings) error {
	name := collapseSpace(req.Values["name"])
	relation := collapseSpace(req.Values["relation"])
	if err := checkName(req.Lang, name); err != nil {
		return err
	}
	if err := checkText(req.Lang, relation, maxRelationRunes, true); err != nil {
		return err
	}
	closeness, err := closenessValue(req.Lang, req.Values["closeness"])
	if err != nil {
		return err
	}
	if closeness == "" {
		closeness = defaultCloseness
	}
	profile := strings.TrimSpace(req.Values["profile"])
	if err := checkText(req.Lang, profile, maxProfileRunes, false); err != nil {
		return err
	}

	ps, err := store.List()
	if err != nil {
		return p.stateFailed(req.Lang, err)
	}
	if personIndex(ps, name) >= 0 {
		return errors.New(i18n.Tf(req.Lang, "plugin.people.state.err.duplicate", "已经认识一个叫 %q 的人了。", name))
	}
	if s.maxPeople > 0 && len(ps) >= s.maxPeople {
		return errors.New(i18n.Tf(req.Lang, "plugin.people.state.err.limit", "人物已达上限（%d 人）。", s.maxPeople))
	}
	_, err = store.Upsert(Update{
		Name: name, Relation: &relation, Profile: &profile, Closeness: &closeness,
	}, time.Now(), Limits{MaxPeople: s.maxPeople})
	if err != nil {
		return p.stateFailed(req.Lang, err)
	}
	return nil
}

func (p *Plugin) editPerson(req plugin.StateEditRequest, store *Store) error {
	ps, err := store.List()
	if err != nil {
		return p.stateFailed(req.Lang, err)
	}
	i := personIndex(ps, req.Row)
	if i < 0 {
		return errors.New(i18n.T(req.Lang, "plugin.people.state.err.no_such", "名册里没有这个人了。"))
	}

	u := Update{Name: ps[i].Name}
	if raw, ok := req.Values["relation"]; ok && req.Touched("relation") {
		v := collapseSpace(raw)
		if err := checkText(req.Lang, v, maxRelationRunes, true); err != nil {
			return err
		}
		u.Relation = &v
	}
	if raw, ok := req.Values["profile"]; ok && req.Touched("profile") {
		v := strings.TrimSpace(raw)
		if err := checkText(req.Lang, v, maxProfileRunes, false); err != nil {
			return err
		}
		u.Profile = &v
	}
	if raw, ok := req.Values["closeness"]; ok && req.Touched("closeness") {
		v, err := closenessValue(req.Lang, raw)
		if err != nil {
			return err
		}
		if v != "" { // 空值表示这次不改这一档
			u.Closeness = &v
		}
	}
	if raw, ok := req.Values["lately"]; ok && req.Touched("lately") {
		v := collapseSpace(raw)
		if err := checkText(req.Lang, v, maxLatelyRunes, false); err != nil {
			return err
		}
		// 与现值相同就不提交：Upsert 会把同一句话再传一次当成「刚听说」，
		// 把过期的近况重新刷成新的。
		if v != ps[i].Lately {
			u.Lately = &v
		}
	}
	if _, err := store.Upsert(u, time.Now(), Limits{MaxPeople: p.snapshot().maxPeople}); err != nil {
		return p.stateFailed(req.Lang, err)
	}
	return nil
}

func (p *Plugin) deletePerson(req plugin.StateEditRequest, store *Store) error {
	ps, err := store.List()
	if err != nil {
		return p.stateFailed(req.Lang, err)
	}
	i := personIndex(ps, req.Row)
	if i < 0 {
		return errors.New(i18n.T(req.Lang, "plugin.people.state.err.no_such", "名册里没有这个人了。"))
	}
	if _, err := store.Remove(ps[i].Name); err != nil {
		return p.stateFailed(req.Lang, err)
	}
	return nil
}

// stateFailed 把库里的错误留给日志，界面只看到一句通用说明：库的报错是写给模型的
// （按进程语言），混进请求语言的界面里会一半中文一半英文。
func (p *Plugin) stateFailed(lang string, err error) error {
	i18n.Logf(p.langOf(), "log.people.state_edit_failed", "状态页修改人物失败：%v", err)
	return errors.New(i18n.T(lang, "plugin.people.state.err.save_failed", "保存人物库失败，请稍后重试。"))
}

func checkName(lang, name string) error {
	switch {
	case name == "":
		return errors.New(i18n.T(lang, "plugin.people.state.err.name_required", "名字不能为空。"))
	case len([]rune(name)) > maxNameRunes:
		return errors.New(i18n.Tf(lang, "plugin.people.state.err.too_long",
			"名字太长（%d 字，上限 %d 字）。", len([]rune(name)), maxNameRunes))
	}
	if _, isSelf := selfNames.Canonical(name); isSelf {
		return errors.New(i18n.T(lang, "plugin.people.state.err.is_self", "对方本人不登记在人物库里。"))
	}
	return nil
}

func checkText(lang, v string, max int, required bool) error {
	if v == "" {
		if required {
			return errors.New(i18n.T(lang, "plugin.people.state.err.text_required", "这一项不能为空。"))
		}
		return nil
	}
	if n := len([]rune(v)); n > max {
		return errors.New(i18n.Tf(lang, "plugin.people.state.err.too_long",
			"内容太长（%d 字，上限 %d 字）。", n, max))
	}
	return nil
}

// closenessValue 把界面送来的亲近度换回落盘规范值；空串表示「这次不改」。
func closenessValue(lang, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	v, ok := closenessLevels.Canonical(raw)
	if !ok {
		return "", errors.New(i18n.T(lang, "plugin.people.state.err.bad_closeness", "亲近度只能是给定的几档。"))
	}
	return v, nil
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

var _ plugin.StateEditor = (*Plugin)(nil)
