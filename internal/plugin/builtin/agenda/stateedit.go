package agenda

import (
	"errors"
	"slices"
	"strings"
	"time"

	"wen/internal/cue"
	"wen/internal/i18n"
	"wen/internal/plugin"
)

// errNoSuchItem / errItemTerminal 是 UpdatePlan 内部用来区分两种拒绝的哨兵，
// 出了锁再按请求语言说人话。
var (
	errNoSuchItem   = errors.New("agenda item not found")
	errItemTerminal = errors.New("agenda item is terminal")
	errStalePlan    = errors.New("agenda plan changed since the page was loaded")
)

// EditState 处理状态页上今天表里某一项的修正：改标题/时间/地点，或取消。
//
// 与模型侧的 update_day_plan 共用同一份表与同一套副作用：时间改了要重算派发时刻，
// 进行中的活动要刷新忙碌公告板，取消要把「快到了」的开口理由撤掉。
// 与对方一起的活动由人在这里改：人就是对方，不需要再商量。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	s := p.snapshot()
	if s.base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.agenda.state.err.not_ready", "今天的安排尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(plugin.ReadDomains(s.base, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.agenda.state.err.unknown_target", "没有要修改的这一块。"))
	}
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.agenda.state.err.not_ready", "今天的安排尚未就绪。"))
	}
	now := p.now()
	day := s.today(now)
	today := day.Format(dateLayout)
	pl, err := store.LoadPlan()
	if err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	if pl.Date != today {
		return errors.New(i18n.T(req.Lang, "plugin.agenda.state.err.no_such", "这一项已经不在了。"))
	}
	version, rev, itemID, ok := splitPlanToken(req.Row)
	if !ok {
		return errors.New(i18n.T(req.Lang, "plugin.agenda.state.err.stale", "这一页已经过期，请刷新后再操作。"))
	}

	switch req.Op {
	case stateOpEdit:
		return p.editItem(req, store, s, day, now, version, rev, itemID)
	case stateOpCancel:
		return p.cancelItem(req, store, s, now, version, rev, itemID)
	}
	return errors.New(i18n.T(req.Lang, "plugin.agenda.state.err.unknown_op", "不认识的修改操作。"))
}

func (p *Plugin) editItem(req plugin.StateEditRequest, store *Store, s settings, day, now time.Time, version, rev int64, itemID string) error {
	title, start, end, place := "", "", "", ""
	if req.Touched("title") {
		title = squash(req.Values["title"])
		if title == "" {
			return errors.New(i18n.T(req.Lang, "plugin.agenda.state.err.title_required", "标题不能为空。"))
		}
		if err := checkStateRunes(req.Lang, title, maxTitleRunes); err != nil {
			return err
		}
	}
	if req.Touched("start") {
		v, err := normHHMM(req.Lang, strings.TrimSpace(req.Values["start"]))
		if err != nil {
			return errors.New(i18n.T(req.Lang, "plugin.agenda.state.err.bad_time", "时间要写成 HH:MM。"))
		}
		start = v
	}
	if req.Touched("end") {
		v, err := normHHMM(req.Lang, strings.TrimSpace(req.Values["end"]))
		if err != nil {
			return errors.New(i18n.T(req.Lang, "plugin.agenda.state.err.bad_time", "时间要写成 HH:MM。"))
		}
		end = v
	}
	if req.Touched("place") {
		place = squash(req.Values["place"])
		if err := checkStateRunes(req.Lang, place, maxPlaceRunes); err != nil {
			return err
		}
	}

	var (
		result      Item
		wasOngoing  bool
		changed     bool
		timeChanged bool
	)
	_, err := store.UpdatePlan(func(pl *Plan) (bool, error) {
		if planVersion(*pl) != version || pl.Rev != rev {
			return false, errStalePlan
		}
		it := pl.item(itemID)
		if it == nil {
			return false, errNoSuchItem
		}
		if it.terminal() {
			return false, errItemTerminal
		}
		wasOngoing = it.Status == statusOngoing
		if req.Touched("title") && title != it.Title {
			it.Title = title
			changed = true
		}
		newStart, newEnd := it.Start, it.End
		if req.Touched("start") {
			newStart = start
		}
		if req.Touched("end") {
			newEnd = end
		}
		if newStart != it.Start || newEnd != it.End {
			it.Start, it.End = newStart, newEnd
			// 时间变了，到点的判断从头来：已派发的记录只对旧时间成立。
			if it.Status == statusPlanned {
				it.StartFired, it.SoonFired = time.Time{}, time.Time{}
			}
			it.EndFired = time.Time{}
			it.BusyCleared = false
			changed, timeChanged = true, true
		}
		if req.Touched("place") && place != it.Place {
			it.Place = place
			changed = true
		}
		result = *it
		return changed, nil
	})
	if err != nil {
		return p.stateEditDecision(req.Lang, err)
	}
	if changed {
		if timeChanged {
			// 旧时间的「快到了」可能已经投出去了，改期必须先撤回，等调度按新时间再投。
			cue.Drop(availabilitySource, "soon|"+result.ID)
		}
		p.afterItemChanged(result, wasOngoing, day, s.dayStartHour, now)
	}
	return nil
}

func (p *Plugin) cancelItem(req plugin.StateEditRequest, store *Store, s settings, now time.Time, version, rev int64, itemID string) error {
	reason := i18n.T(s.lang, "plugin.agenda.state.cancel_reason", "在设置页取消")
	var (
		result     Item
		wasOngoing bool
	)
	_, err := store.UpdatePlan(func(pl *Plan) (bool, error) {
		if planVersion(*pl) != version || pl.Rev != rev {
			return false, errStalePlan
		}
		it := pl.item(itemID)
		if it == nil {
			return false, errNoSuchItem
		}
		if it.terminal() {
			return false, errItemTerminal
		}
		wasOngoing = it.Status == statusOngoing
		it.Status, it.Outcome = statusCancelled, reason
		result = *it
		return true, nil
	})
	if err != nil {
		return p.stateEditDecision(req.Lang, err)
	}
	p.afterItemChanged(result, wasOngoing, time.Time{}, 0, now)
	return nil
}

// afterItemChanged 是与模型侧 update_day_plan 相同的那几件收尾事：进行中的活动
// 刷新忙碌公告板，结束了的撤掉，「快到了」的开口理由撤掉，唤醒调度重算下一次到期。
// day 为零值时只清不设（取消的项已经不是进行中了）。
func (p *Plugin) afterItemChanged(it Item, wasOngoing bool, day time.Time, dayStartHour int, now time.Time) {
	switch {
	case it.Status == statusOngoing && !day.IsZero():
		clearAvailability()
		setAvailability(&it, day, dayStartHour, now)
		cue.Drop(availabilitySource, "soon|"+it.ID)
	case it.terminal():
		if wasOngoing {
			clearAvailability()
		}
		cue.Drop(availabilitySource, "soon|"+it.ID)
	}
	p.wakeup()
}

func (p *Plugin) stateEditDecision(lang string, err error) error {
	switch {
	case errors.Is(err, errStalePlan):
		return errors.New(i18n.T(lang, "plugin.agenda.state.err.stale", "这一页已经过期，请刷新后再操作。"))
	case errors.Is(err, errNoSuchItem):
		return errors.New(i18n.T(lang, "plugin.agenda.state.err.no_such", "这一项已经不在了。"))
	case errors.Is(err, errItemTerminal):
		return errors.New(i18n.T(lang, "plugin.agenda.state.err.terminal", "这一项已经结束，不能再改。"))
	}
	return p.stateEditFailed(lang, err)
}

func (p *Plugin) stateEditFailed(lang string, err error) error {
	i18n.Logf(p.langOf(), "log.agenda.state_edit_failed", "状态页修改日程失败：%v", err)
	return errors.New(i18n.T(lang, "plugin.agenda.state.err.save_failed", "保存日程失败，请稍后重试。"))
}

func checkStateRunes(lang, v string, max int) error {
	if n := len([]rune(v)); n > max {
		return errors.New(i18n.Tf(lang, "plugin.agenda.state.err.too_long",
			"内容太长（%d 字，上限 %d 字）。", n, max))
	}
	return nil
}

var _ plugin.StateEditor = (*Plugin)(nil)
