package agenda

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"wen/internal/cue"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// errNotReady 在插件未取得持久化目录时返回，正常流程下不会出现。
func errNotReady(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.agenda.result.not_ready", "日程尚未就绪"))
}

// userRuleZH 是「与对方的约定不能单方面改」的拒绝措辞。拒绝的同时把规则与出口都告诉模型，
// 否则它只会换个说法再试一次。
const userRuleZH = "「%s」是和对方的约定，改动要先跟对方商量；对方在本轮对话里明确同意后，再传 agreed_by_user: true 重试"

// checkWith 校验「和谁」：去重、限量，每个名字都得是人物库里的人，并换成库里的规范写法。
func (p *Plugin) checkWith(ctx context.Context, lang string, with []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, w := range with {
		w = squash(w)
		if w == "" {
			continue
		}
		if p.lookup != nil {
			canonical, ok := p.lookup.Known(ctx, w)
			if !ok {
				known := p.lookup.Names(ctx)
				if len(known) == 0 {
					return nil, fmt.Errorf(prompt.T(lang, "prompt.agenda.result.no_person_empty",
						"没有叫「%s」的人物，人物清单还是空的。先用 upsert_person 登记他"), w)
				}
				return nil, fmt.Errorf(prompt.T(lang, "prompt.agenda.result.no_person",
					"没有叫「%s」的人物，已登记的有：%s。先用 upsert_person 登记他"),
					w, strings.Join(known, prompt.T(lang, "prompt.agenda.render.name_sep", "、")))
			}
			w = canonical
		}
		if seen[strings.ToLower(w)] {
			continue
		}
		seen[strings.ToLower(w)] = true
		out = append(out, w)
	}
	if len(out) > maxWith {
		return nil, fmt.Errorf("一项最多写 %d 个同行的人", maxWith)
	}
	return out, nil
}

// ---------- set_day_plan ----------

type setPlanTool struct{ p *Plugin }

func (t *setPlanTool) Name() string { return "set_day_plan" }

func (t *setPlanTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.agenda.tool.set_day_plan.desc",
		"为今天排一张日程表：2-4 件事，中间留大段空白，每项给开始结束时间、和谁、能不能挪。"+
			"今天的未来约定必须全部排进去（填 from_commitment）。整表覆盖，只在【规划今天】的轮次里使用，"+
			"平时改动用 update_day_plan，临时加一件今天的事用 add_commitment。")
}

func (t *setPlanTool) Schema() json.RawMessage {
	lang := t.p.langOf()
	return json.RawMessage(prompt.Tf(lang, "prompt.agenda.tool.set_day_plan.schema",
		setdayplanSchemaZH, flexLevels.JSON(lang), busyLevels.JSON(lang)))
}

type planItemArg struct {
	Title          string   `json:"title"`
	Start          string   `json:"start"`
	End            string   `json:"end"`
	Place          string   `json:"place"`
	With           []string `json:"with"`
	WithUser       bool     `json:"with_user"`
	Flex           string   `json:"flex"`
	Busy           string   `json:"busy"`
	FromCommitment string   `json:"from_commitment"`
}

// buildItem 把一条参数校验并规整成 Item（不含 id 与状态）。
func (p *Plugin) buildItem(ctx context.Context, lang string, a planItemArg) (Item, error) {
	it := Item{Title: squash(a.Title), Place: squash(a.Place), WithUser: a.WithUser,
		Flex: strings.TrimSpace(a.Flex), Busy: strings.TrimSpace(a.Busy), FromCommitment: strings.TrimSpace(a.FromCommitment)}
	if it.Title == "" {
		// 提一句最常见的成因：模型偶尔会把一件事拆成两个对象（前一半带 title 与时间，
		// 后一半只剩 place / flex / busy），报「title 不能为空」的话它多半去改别的地方
		return it, fmt.Errorf("%s", prompt.T(lang, "prompt.agenda.result.no_title",
			"有一项没有 title：每件事是一个完整的对象，"+
				"title、start、end、flex 要写在同一个对象里，不要拆成两段"))
	}
	if err := checkRunes(lang, it.Title, "title", maxTitleRunes); err != nil {
		return it, err
	}
	if err := checkRunes(lang, it.Place, "place", maxPlaceRunes); err != nil {
		return it, err
	}
	var err error
	if it.Start, err = normHHMM(lang, a.Start); err != nil {
		return it, fmt.Errorf(prompt.T(lang, "prompt.agenda.result.item_start", "「%s」的开始%w"), it.Title, err)
	}
	if it.End, err = normHHMM(lang, a.End); err != nil {
		return it, fmt.Errorf(prompt.T(lang, "prompt.agenda.result.item_end", "「%s」的结束%w"), it.Title, err)
	}
	if it.With, err = p.checkWith(ctx, lang, a.With); err != nil {
		return it, err
	}
	if it.WithUser {
		it.Flex = flexFixed // 与对方的约定一律不能动
	}
	// 「缺了」与「填错了」分开说：都报「只能是……」的话，缺字段的那次会被读成
	// 「填的值不对」，模型于是去改一个本来就没写的字段，白绕两圈
	zhFlex, okFlex := canonFlex(it.Flex)
	if !okFlex {
		return it, fmt.Errorf(prompt.T(lang, "prompt.agenda.result.item_enum", "「%s」%s：%s"),
			it.Title, missingOrBad(lang, it.Flex, "flex"), strings.Join(flexLevels.Values(lang), " / "))
	}
	it.Flex = zhFlex // 落盘的是中文规范值
	if it.Busy == "" {
		it.Busy = defaultBusy
	}
	zhBusy, okBusy := canonBusy(it.Busy)
	if !okBusy {
		return it, fmt.Errorf(prompt.T(lang, "prompt.agenda.result.item_enum", "「%s」%s：%s"),
			it.Title, missingOrBad(lang, it.Busy, "busy"), strings.Join(busyLevels.Values(lang), " / "))
	}
	it.Busy = zhBusy
	return it, nil
}

// missingOrBad 按字段是空还是填错，给出前半句。
func missingOrBad(lang, value, field string) string {
	if strings.TrimSpace(value) == "" {
		return prompt.Tf(lang, "prompt.agenda.result.missing_field", "缺少 %s，只能是", field)
	}
	return prompt.Tf(lang, "prompt.agenda.result.bad_field", "的 %s 只能是", field)
}

// overlapNotes 找出时间重叠的项。有重叠不拒绝，只告知——到时候模型自己取舍。
func overlapNotes(lang string, items []Item, day time.Time, dayStartHour int) []string {
	var notes []string
	for i := 0; i < len(items); i++ {
		si, ei := items[i].span(day, dayStartHour)
		for j := i + 1; j < len(items); j++ {
			sj, ej := items[j].span(day, dayStartHour)
			if si.Before(ej) && sj.Before(ei) {
				notes = append(notes, prompt.Tf(lang, "prompt.agenda.result.overlap",
					"%s-%s 与 %s-%s 重叠，留着也行，到时你自己取舍",
					items[i].Start, items[i].End, items[j].Start, items[j].End))
			}
		}
	}
	return notes
}

func (t *setPlanTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Items []planItemArg `json:"items"`
		Note  string        `json:"note"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}
	p := t.p
	s := p.snapshot()
	st := p.writeStore(ctx)
	if st == nil {
		return "", errNotReady(s.lang)
	}
	// 规划轮次里已经提交过就不再受理：那句「排好后用 set_day_plan 提交」是一次性输入，
	// 却在工具循环的每次迭代里被重新读到，于是模型一版一版地微调着重提交
	//（实测连提十三次，见 planSubmitted 的说明）。
	tag := plugin.ScopeFrom(ctx).Write
	p.mu.RLock()
	resubmit := p.planning[tag] && p.planSubmitted[tag]
	p.mu.RUnlock()
	if resubmit {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.agenda.result.resubmit",
			"今天的表刚刚已经排好了（见上一条结果），本轮不要再提交。"+
				"要改其中某一项用 update_day_plan；没有要改的就直接结束这一轮"))
	}
	if len(a.Items) > s.maxItems {
		return "", fmt.Errorf("一天最多排 %d 项（现在 %d 项），精简后重试；提示词要求的是 2-4 件事，留大段空白", s.maxItems, len(a.Items))
	}
	now := p.now()
	day := s.today(now)
	today := day.Format(dateLayout)

	cs, err := st.LoadCommitments()
	if err != nil {
		return "", err
	}
	todayCs := map[string]Commitment{}
	for _, c := range cs {
		if c.Date == today {
			todayCs[c.ID] = c
		}
	}

	// 先整体扫一眼有没有缺 title 的对象。逐项校验会先在前一项上报出「缺少 flex」，
	// 而那正是拆项的后遗症——真正的毛病在下一个对象上，先说这一条模型才改得对。
	for _, arg := range a.Items {
		if strings.TrimSpace(arg.Title) == "" {
			return "", fmt.Errorf("有一项没有 title：每件事是一个完整的对象，" +
				"title、start、end、flex 要写在同一个对象里，不要拆成两段")
		}
	}

	items := make([]Item, 0, len(a.Items))
	covered := map[string]bool{}
	for _, arg := range a.Items {
		it, err := p.buildItem(ctx, s.lang, arg)
		if err != nil {
			return "", err
		}
		if it.FromCommitment != "" {
			c, ok := todayCs[it.FromCommitment]
			if !ok {
				return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.no_commitment",
					"今天没有 id 为 %s 的约定，可用 list_commitments 查看"), it.FromCommitment)
			}
			if covered[c.ID] {
				return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.commitment_twice",
					"约定 %s 被排了两次"), c.ID)
			}
			covered[c.ID] = true
			if c.WithUser {
				it.WithUser, it.Flex = true, flexFixed
				if it.Start != c.Start || (c.End != "" && it.End != c.End) {
					return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.commitment_fixed",
						"约定 %s 是和对方的，时间 %s%s 不能改；要改先跟对方商量"), c.ID, c.Start, dashEnd(c.End))
				}
			}
			if len(it.With) == 0 {
				it.With = c.With
			}
			if it.Place == "" {
				it.Place = c.Place
			}
		}
		items = append(items, it)
	}
	var missing []string
	for _, c := range cs {
		if c.Date == today && !covered[c.ID] {
			missing = append(missing, fmt.Sprintf("[%s] %s%s %s", c.ID, c.Start, dashEnd(c.End), c.Title))
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.commitment_unplanned",
			"今天有约定 %s 还没排进表，补上它（填 from_commitment）或先用 cancel_commitment 取消"),
			strings.Join(missing, prompt.T(s.lang, "prompt.agenda.render.name_sep", "、")))
	}

	pl, err := st.UpdatePlan(func(pl *Plan) (bool, error) {
		replan := pl.Date == today
		old := pl.Items
		for i := range items {
			items[i].ID = fmt.Sprintf("a%d", i+1)
			items[i].Status = statusPlanned
			if replan {
				// 重排只清状态，不清派发记录：同一件事到点已经派过开始轮次，不该再派一次
				for _, o := range old {
					if (o.FromCommitment != "" && o.FromCommitment == items[i].FromCommitment) ||
						(o.Title == items[i].Title && o.Start == items[i].Start) {
						items[i].StartFired, items[i].EndFired, items[i].SoonFired = o.StartFired, o.EndFired, o.SoonFired
						break
					}
				}
			}
		}
		pl.Date, pl.Weekday, pl.PlannedAt = today, weekdayName(s.lang, day), now
		pl.Note = squash(a.Note)
		pl.Items = items
		return true, nil
	})
	if err != nil {
		return "", err
	}
	clearAvailability() // 重排后没有「进行中」的项
	if len(covered) > 0 {
		_, _ = st.UpdateCommitments(func(list *[]Commitment, _ func() string) (bool, error) {
			for i := range *list {
				if covered[(*list)[i].ID] {
					(*list)[i].Planned = true
				}
			}
			return true, nil
		})
	}
	p.wakeup()
	p.mu.Lock()
	p.planSubmitted[tag] = true
	p.mu.Unlock()
	log.Printf("agenda: 今天（%s）的表已排定，%d 项", today, len(pl.Items))

	if len(pl.Items) == 0 {
		return prompt.T(s.lang, "prompt.agenda.result.plan_empty",
			"今天没有安排，整天空着。表已排定，这一轮到此为止，不要再次提交。"), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, prompt.T(s.lang, "prompt.agenda.result.plan_head", "今天（%s）的安排已定，共 %d 项：")+"\n",
		pl.Weekday, len(pl.Items))
	for _, it := range pl.Items {
		b.WriteString(itemLine(s.lang, it) + "\n")
	}
	for _, n := range overlapNotes(s.lang, pl.Items, day, s.dayStartHour) {
		b.WriteString(n + "\n")
	}
	// 终结语。只回一份状态清单的话，模型会把它读成进展汇报，接着微调重排——
	// 成功的回执要自己说清「做完了」。
	b.WriteString(prompt.T(s.lang, "prompt.agenda.result.plan_done",
		"表已排定，这一轮到此为止：不要再次提交，也不必再调用别的工具。") + "\n")
	return strings.TrimRight(b.String(), "\n"), nil
}

// itemLine 写一项：`a2 14:00-16:30 和林舟在图书馆查资料（尽量守，来自约定 c7）`。
func itemLine(lang string, it Item) string {
	tags := flexLevels.For(lang, it.Flex)
	if it.FromCommitment != "" {
		tags += prompt.Tf(lang, "prompt.agenda.result.from_commitment", "，来自约定 %s", it.FromCommitment)
	}
	return prompt.Tf(lang, "prompt.agenda.result.item_line", "%s %s-%s %s（%s）",
		it.ID, it.Start, it.End, it.Title, tags)
}

// ---------- update_day_plan ----------

type updatePlanTool struct{ p *Plugin }

func (t *updatePlanTool) Name() string { return "update_day_plan" }

func (t *updatePlanTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.agenda.tool.update_day_plan.desc",
		"更新今天日程里的一项：开始了、做完了、挪时间、改做法、延期到别天、跳过或取消。"+
			"做完写一句经历，跳过、取消、延期写原因。与对方的约定不能由你单方面改，先商量；"+
			"agreed_by_user 只在对方在本轮对话里明确同意后才传。")
}

func (t *updatePlanTool) Schema() json.RawMessage {
	lang := t.p.langOf()
	return json.RawMessage(prompt.Tf(lang, "prompt.agenda.tool.update_day_plan.schema",
		updatedayplanSchemaZH, busyLevels.JSON(lang)))
}

func (t *updatePlanTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID      string  `json:"id"`
		Status  string  `json:"status"`
		Start   *string `json:"start"`
		End     *string `json:"end"`
		Title   *string `json:"title"`
		Busy    *string `json:"busy"`
		Outcome *string `json:"outcome"`
		DeferTo string  `json:"defer_to"`
		Agreed  bool    `json:"agreed_by_user"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}
	p := t.p
	s := p.snapshot()
	if s.base == "" {
		return "", errNotReady(s.lang)
	}
	a.ID = strings.TrimSpace(a.ID)
	loc, ok := p.findItem(ctx, a.ID)
	if !ok {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.no_item_list",
			"没有 id 为 %q 的项，今天的表里有：%s"), a.ID, p.idList(ctx))
	}
	st := p.storeFor(loc.tag)
	now := p.now()
	day := s.today(now)
	today := day.Format(dateLayout)

	var (
		changes    []string
		deferred   *Commitment
		wasOngoing bool
		result     Item
	)
	_, err := st.UpdatePlan(func(pl *Plan) (bool, error) {
		it := pl.item(a.ID)
		if it == nil {
			return false, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.no_item", "没有 id 为 %q 的项"), a.ID)
		}
		wasOngoing = it.Status == statusOngoing
		timeChange := a.Start != nil || a.End != nil
		unilateral := timeChange || a.Status == statusSkipped || a.Status == statusDeferred || a.Status == statusCancelled
		if it.WithUser && unilateral && !a.Agreed {
			return false, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.user_rule", userRuleZH), it.Title)
		}
		if it.terminal() && (timeChange || a.Status != "" || a.Title != nil || a.Busy != nil) {
			return false, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.terminal",
				"「%s」已经%s，不能再改；只有 outcome 还可以补"), it.Title, statusCN(s.lang, it.Status))
		}

		if a.Title != nil {
			title := squash(*a.Title)
			if title == "" {
				return false, fmt.Errorf("%s", prompt.T(s.lang, "prompt.agenda.result.title_empty", "title 不能为空"))
			}
			if err := checkRunes(s.lang, title, "title", maxTitleRunes); err != nil {
				return false, err
			}
			if title != it.Title {
				changes = append(changes, prompt.Tf(s.lang, "prompt.agenda.change.title", "改为「%s」", title))
				it.Title = title
			}
		}
		if timeChange {
			start, end := it.Start, it.End
			var err error
			if a.Start != nil {
				if start, err = normHHMM(s.lang, *a.Start); err != nil {
					return false, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.start_prefix", "开始%w"), err)
				}
			}
			if a.End != nil {
				if end, err = normHHMM(s.lang, *a.End); err != nil {
					return false, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.end_prefix", "结束%w"), err)
				}
			}
			if start != it.Start || end != it.End {
				it.Start, it.End = start, end
				changes = append(changes, prompt.Tf(s.lang, "prompt.agenda.change.time", "时间改为 %s-%s", start, end))
				// 时间变了，到点的判断从头来：已派发的记录只对旧时间成立
				if it.Status == statusPlanned {
					it.StartFired, it.SoonFired = time.Time{}, time.Time{}
				}
				it.EndFired = time.Time{}
			}
		}
		if a.Busy != nil {
			b, okBusy := canonBusy(strings.TrimSpace(*a.Busy))
			if !okBusy {
				return false, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.bad_busy", "busy 只能是：%s"),
					strings.Join(busyLevels.Values(s.lang), " / "))
			}
			if b != it.Busy {
				it.Busy = b
				changes = append(changes, prompt.Tf(s.lang, "prompt.agenda.change.busy", "期间%s",
					busyLevels.For(s.lang, b)))
			}
		}
		outcome := ""
		if a.Outcome != nil {
			outcome = squash(*a.Outcome)
			if err := checkRunes(s.lang, outcome, "outcome", maxOutcomeRunes); err != nil {
				return false, err
			}
		}
		switch a.Status {
		case "":
			if a.Outcome != nil && outcome != it.Outcome {
				it.Outcome = outcome
				changes = append(changes, prompt.T(s.lang, "prompt.agenda.change.outcome", "经历已更新"))
			}
		case statusOngoing:
			it.Status = statusOngoing
			if it.StartFired.IsZero() {
				it.StartFired = now
			}
			changes = append(changes, prompt.Tf(s.lang, "prompt.agenda.change.ongoing", "已标为进行中（到 %s）", it.End))
		case statusDone:
			it.Status, it.Outcome = statusDone, outcome
			if outcome == "" {
				changes = append(changes, prompt.T(s.lang, "prompt.agenda.change.done_bare",
					"已做完（没写经历，下次记得写一句）"))
			} else {
				changes = append(changes, prompt.Tf(s.lang, "prompt.agenda.change.done", "已做完：%s", outcome))
			}
		case statusSkipped, statusCancelled:
			if outcome == "" {
				return false, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.need_reason",
					"%s要写原因（outcome）"), statusCN(s.lang, a.Status))
			}
			it.Status, it.Outcome = a.Status, outcome
			changes = append(changes, prompt.Tf(s.lang, "prompt.agenda.change.status_reason", "%s：%s",
				statusCN(s.lang, a.Status), outcome))
		case statusDeferred:
			if outcome == "" {
				return false, fmt.Errorf("%s", prompt.T(s.lang, "prompt.agenda.result.defer_needs_reason",
					"延期要写原因（outcome）"))
			}
			date, hhmm, err := parseDeferTo(s.lang, a.DeferTo, now.Location())
			if err != nil {
				return false, err
			}
			if date <= today {
				return false, fmt.Errorf("%s", prompt.T(s.lang, "prompt.agenda.result.defer_future",
					"延期的日期要在今天之后；今天之内挪时间直接改 start / end"))
			}
			it.Status, it.Outcome = statusDeferred, outcome
			changes = append(changes, prompt.Tf(s.lang, "prompt.agenda.change.deferred", "已延期：%s", outcome))
			c := Commitment{Date: date, Start: it.Start, End: it.End, Title: it.Title, With: it.With,
				WithUser: it.WithUser, Place: it.Place, Flex: it.Flex, Created: now,
				Note: prompt.Tf(s.lang, "prompt.agenda.change.defer_note", "从 %d/%d 延期：%s",
					int(day.Month()), day.Day(), outcome)}
			if hhmm != "" {
				c.Start, c.End = hhmm, ""
			}
			deferred = &c
		default:
			return false, fmt.Errorf("%s", prompt.T(s.lang, "prompt.agenda.result.bad_status",
				"status 只能是：ongoing / done / skipped / deferred / cancelled"))
		}
		result = *it
		return len(changes) > 0, nil
	})
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return prompt.Tf(s.lang, "prompt.agenda.result.item_no_change", "%s 没有变化。", a.ID), nil
	}

	// 忙碌状态与开口理由跟着状态走
	switch {
	case result.Status == statusOngoing:
		clearAvailability()
		setAvailability(&result, day, s.dayStartHour, now)
		cue.Drop(availabilitySource, "soon|"+result.ID)
	case result.terminal():
		if wasOngoing {
			clearAvailability()
		}
		cue.Drop(availabilitySource, "soon|"+result.ID)
	}
	if deferred != nil {
		_, err := st.UpdateCommitments(func(list *[]Commitment, next func() string) (bool, error) {
			if len(*list) >= maxCommitments {
				return false, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.commit_limit",
					"约定已达上限（%d 条），先清理过期的"), maxCommitments)
			}
			deferred.ID = next()
			*list = append(*list, *deferred)
			return true, nil
		})
		if err != nil {
			return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.defer_commit_failed",
				"项已标为延期，但登记约定失败: %w"), err)
		}
		changes = append(changes, prompt.Tf(s.lang, "prompt.agenda.change.defer_to", "已延期到 %s %s，登记为约定 %s",
			fmtDateCN(s.lang, deferred.Date, now.Location()), deferred.Start, deferred.ID))
	}
	p.wakeup()
	sep := prompt.T(s.lang, "prompt.agenda.render.item_sep", "；")
	log.Printf("agenda: %s %s", a.ID, strings.Join(changes, sep))
	return prompt.Tf(s.lang, "prompt.agenda.result.item_changed", "%s %s。", a.ID, strings.Join(changes, sep)), nil
}

// idList 列出本轮写入域今天表里的项，供报错时参考。
func (p *Plugin) idList(ctx context.Context) string {
	lang := p.langOf()
	empty := prompt.T(lang, "prompt.agenda.render.empty", "（空）")
	st := p.writeStore(ctx)
	if st == nil {
		return empty
	}
	pl, err := st.LoadPlan()
	if err != nil || len(pl.Items) == 0 {
		return empty
	}
	parts := make([]string, 0, len(pl.Items))
	for _, it := range pl.Items {
		parts = append(parts, it.ID+" "+it.Title)
	}
	return strings.Join(parts, prompt.T(lang, "prompt.agenda.render.name_sep", "、"))
}

func statusCN(lang, st string) string {
	switch st {
	case statusOngoing:
		return prompt.T(lang, "prompt.agenda.status.ongoing", "进行中")
	case statusDone:
		return prompt.T(lang, "prompt.agenda.status.done", "做完")
	case statusSkipped:
		return prompt.T(lang, "prompt.agenda.status.skipped", "跳过")
	case statusDeferred:
		return prompt.T(lang, "prompt.agenda.status.deferred", "延期")
	case statusCancelled:
		return prompt.T(lang, "prompt.agenda.status.cancelled", "取消")
	}
	return st
}

// parseDeferTo 解析 "YYYY-MM-DD" 或 "YYYY-MM-DD HH:MM"。
func parseDeferTo(lang, s string, loc *time.Location) (date, hhmm string, err error) {
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields) > 2 {
		return "", "", fmt.Errorf("%s", prompt.T(lang, "prompt.agenda.result.bad_defer_to",
			"defer_to 应为 YYYY-MM-DD，可带 HH:MM"))
	}
	d, err := parseDate(lang, fields[0], loc)
	if err != nil {
		return "", "", err
	}
	date = d.Format(dateLayout)
	if len(fields) == 2 {
		if hhmm, err = normHHMM(lang, fields[1]); err != nil {
			return "", "", err
		}
	}
	return date, hhmm, nil
}

// ---------- add_commitment ----------

type addCommitmentTool struct{ p *Plugin }

func (t *addCommitmentTool) Name() string { return "add_commitment" }

func (t *addCommitmentTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.agenda.tool.add_commitment.desc",
		"登记一件定在某天某时的事：和对方约好的、答应别人的、自己要去办的。有具体日期时刻才用它；"+
			"传今天的日期时直接加进今天的表。对方要你到点提醒他的事用定时任务，不在这里登记。")
}

func (t *addCommitmentTool) Schema() json.RawMessage {
	lang := t.p.langOf()
	return json.RawMessage(prompt.Tf(lang, "prompt.agenda.tool.add_commitment.schema",
		addcommitmentSchemaZH, flexLevels.JSON(lang)))
}

func (t *addCommitmentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Date     string   `json:"date"`
		Start    string   `json:"start"`
		End      string   `json:"end"`
		Title    string   `json:"title"`
		With     []string `json:"with"`
		WithUser bool     `json:"with_user"`
		Place    string   `json:"place"`
		Note     string   `json:"note"`
		Flex     string   `json:"flex"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}
	p := t.p
	s := p.snapshot()
	st := p.writeStore(ctx)
	if st == nil {
		return "", errNotReady(s.lang)
	}
	now := p.now()
	day := s.today(now)
	today := day.Format(dateLayout)

	d, err := parseDate(s.lang, a.Date, now.Location())
	if err != nil {
		return "", err
	}
	c := Commitment{Date: d.Format(dateLayout), Title: squash(a.Title), Place: squash(a.Place),
		Note: squash(a.Note), WithUser: a.WithUser, Flex: strings.TrimSpace(a.Flex), Created: now}
	if c.Date < today {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.date_past",
			"%s 已经过去了，约定只能登记今天或之后的日子"), fmtDateCN(s.lang, c.Date, now.Location()))
	}
	if c.Title == "" {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.agenda.result.title_empty", "title 不能为空"))
	}
	if err := checkRunes(s.lang, c.Title, "title", maxTitleRunes); err != nil {
		return "", err
	}
	if err := checkRunes(s.lang, c.Place, "place", maxPlaceRunes); err != nil {
		return "", err
	}
	if err := checkRunes(s.lang, c.Note, "note", maxNoteRunes); err != nil {
		return "", err
	}
	if c.Start, err = normHHMM(s.lang, a.Start); err != nil {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.start_prefix", "开始%w"), err)
	}
	if strings.TrimSpace(a.End) != "" {
		if c.End, err = normHHMM(s.lang, a.End); err != nil {
			return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.end_prefix", "结束%w"), err)
		}
	}
	if c.With, err = p.checkWith(ctx, s.lang, a.With); err != nil {
		return "", err
	}
	switch zh, okFlex := canonFlex(c.Flex); {
	case c.WithUser:
		c.Flex = flexFixed
	case c.Flex == "":
		c.Flex = flexTry
	case !okFlex:
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.bad_flex", "flex 只能是：%s"),
			strings.Join(flexLevels.Values(s.lang), " / "))
	default:
		c.Flex = zh // 落盘的是中文规范值
	}

	// 今天的约定：表已排定就直接追加进表，否则留在约定里等规划时排入
	var appended *Item
	if c.Date == today {
		pl, err := st.LoadPlan()
		if err != nil {
			return "", err
		}
		if pl.Date == today {
			if len(pl.Items) >= hardMaxItems {
				return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.plan_full",
					"今天的表已有 %d 项，不能再加"), len(pl.Items))
			}
			c.Planned = true
			it := Item{Title: c.Title, Start: c.Start, End: c.End, Place: c.Place, With: c.With,
				WithUser: c.WithUser, Flex: c.Flex, Busy: defaultBusy, Status: statusPlanned}
			if it.End == "" {
				it.End = it.Start // 没说到几点：按「到点就算」处理，模型可再改
			}
			appended = &it
		}
	}
	_, err = st.UpdateCommitments(func(list *[]Commitment, next func() string) (bool, error) {
		if len(*list) >= maxCommitments {
			return false, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.commit_limit_cancel",
				"约定已达上限（%d 条），先用 cancel_commitment 清掉不再成立的"), maxCommitments)
		}
		c.ID = next()
		*list = append(*list, c)
		return true, nil
	})
	if err != nil {
		return "", err
	}
	var overlap []string
	if appended != nil {
		appended.FromCommitment = c.ID
		pl, err := st.UpdatePlan(func(pl *Plan) (bool, error) {
			appended.ID = fmt.Sprintf("a%d", len(pl.Items)+1)
			pl.Items = append(pl.Items, *appended)
			return true, nil
		})
		if err != nil {
			return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.commit_added_plan_failed",
				"约定已登记，但加进今天的表失败: %w"), err)
		}
		overlap = overlapNotes(s.lang, pl.Items, day, s.dayStartHour)
	}
	p.wakeup()
	log.Printf("agenda: 登记约定 %s：%s %s %s", c.ID, c.Date, c.Start, c.Title)

	desc := prompt.Tf(s.lang, "prompt.agenda.result.commit_desc", "%s %s%s %s（%s）",
		fmtDateCN(s.lang, c.Date, now.Location()), c.Start, dashEnd(c.End), c.Title,
		flexLevels.For(s.lang, c.Flex))
	if appended != nil {
		out := prompt.Tf(s.lang, "prompt.agenda.result.commit_added_today",
			"已登记约定 %s 并加进今天的表（%s）：%s。", c.ID, appended.ID, desc)
		if len(overlap) > 0 {
			out += "\n" + strings.Join(overlap, "\n")
		}
		return out, nil
	}
	if c.Date == today {
		return fmt.Sprintf("已登记约定 %s：%s。今天的表还没排，排表时会要求把它排进去。", c.ID, desc), nil
	}
	return fmt.Sprintf("已登记约定 %s：%s。", c.ID, desc), nil
}

// ---------- cancel_commitment ----------

type cancelCommitmentTool struct{ p *Plugin }

func (t *cancelCommitmentTool) Name() string { return "cancel_commitment" }

func (t *cancelCommitmentTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.agenda.tool.cancel_commitment.desc",
		"取消一条约定，要写原因。与对方的约定不能由你单方面取消，先商量；"+
			"agreed_by_user 只在对方在本轮对话里明确同意后才传。")
}

func (t *cancelCommitmentTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.agenda.tool.cancel_commitment.schema",
		cancelcommitmentSchemaZH))
}

func (t *cancelCommitmentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
		Agreed bool   `json:"agreed_by_user"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}
	p := t.p
	s := p.snapshot()
	if s.base == "" {
		return "", errNotReady(s.lang)
	}
	a.ID = strings.TrimSpace(a.ID)
	reason := squash(a.Reason)
	if reason == "" {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.agenda.result.cancel_needs_reason",
			"reason 不能为空：一句话说明为什么取消"))
	}
	if err := checkRunes(s.lang, reason, "reason", maxOutcomeRunes); err != nil {
		return "", err
	}

	// 约定在可读域里找（写入域优先），写回它所在的域
	var (
		st      *Store
		removed Commitment
		found   bool
	)
	for _, tag := range p.readDomains(ctx) {
		cand := p.storeFor(tag)
		if cand == nil {
			continue
		}
		_, err := cand.UpdateCommitments(func(list *[]Commitment, _ func() string) (bool, error) {
			for i, c := range *list {
				if c.ID != a.ID {
					continue
				}
				if c.WithUser && !a.Agreed {
					return false, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.user_rule", userRuleZH), c.Title)
				}
				removed, found = c, true
				*list = append((*list)[:i], (*list)[i+1:]...)
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			return "", err
		}
		if found {
			st = cand
			break
		}
	}
	if !found {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.no_commitment_id",
			"没有 id 为 %q 的约定，可用 list_commitments 查看"), a.ID)
	}
	// 已排进今天表里的，表上那一项一并取消
	var itemID string
	wasOngoing := false
	_, _ = st.UpdatePlan(func(pl *Plan) (bool, error) {
		for i := range pl.Items {
			it := &pl.Items[i]
			if it.FromCommitment == removed.ID && !it.terminal() {
				wasOngoing = it.Status == statusOngoing
				it.Status, it.Outcome = statusCancelled, reason
				itemID = it.ID
				return true, nil
			}
		}
		return false, nil
	})
	if itemID != "" {
		cue.Drop(availabilitySource, "soon|"+itemID)
		if wasOngoing {
			clearAvailability()
		}
	}
	p.wakeup()
	log.Printf("agenda: 取消约定 %s（%s）：%s", removed.ID, removed.Title, reason)
	out := prompt.Tf(s.lang, "prompt.agenda.result.commit_cancelled", "已取消约定 %s（%s %s %s）：%s。",
		removed.ID, fmtDateCN(s.lang, removed.Date, p.now().Location()), removed.Start, removed.Title, reason)
	if itemID != "" {
		out += prompt.Tf(s.lang, "prompt.agenda.result.item_cancelled_too", "今天表里的 %s 一并标为取消。", itemID)
	}
	return out, nil
}

// ---------- list_commitments ----------

type listCommitmentsTool struct{ p *Plugin }

func (t *listCommitmentsTool) Name() string { return "list_commitments" }

func (t *listCommitmentsTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.agenda.tool.list_commitments.desc",
		"列出全部已登记的约定（含备注与同行的人），可只看某一天。用于注入的约定不全、或要核对某天的安排时。")
}

func (t *listCommitmentsTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.agenda.tool.list_commitments.schema",
		listcommitmentsSchemaZH))
}

func (t *listCommitmentsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Date string `json:"date"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", plugin.BadArgs(t.p.langOf(), err)
		}
	}
	p := t.p
	s := p.snapshot()
	st := p.writeStore(ctx)
	if st == nil {
		return "", errNotReady(s.lang)
	}
	now := p.now()
	filter := ""
	if strings.TrimSpace(a.Date) != "" {
		d, err := parseDate(s.lang, a.Date, now.Location())
		if err != nil {
			return "", err
		}
		filter = d.Format(dateLayout)
	}
	cs, err := st.LoadCommitments()
	if err != nil {
		return "", err
	}
	if len(cs) == 0 {
		return prompt.T(s.lang, "prompt.agenda.result.no_commitments", "没有登记任何约定。"), nil
	}
	sortCommitments(cs)
	var b strings.Builder
	matched := 0
	for _, c := range cs {
		if filter != "" && c.Date != filter {
			continue
		}
		matched++
		fmt.Fprintf(&b, prompt.T(s.lang, "prompt.agenda.list.commit_line", "- [%s] %s %s%s %s（%s）"),
			c.ID, fmtDateShort(s.lang, c.Date, now.Location()), c.Start, dashEnd(c.End), c.Title,
			flexLevels.For(s.lang, c.Flex))
		if c.WithUser {
			b.WriteString(prompt.T(s.lang, "prompt.agenda.list.with_user", "，和对方"))
		}
		if len(c.With) > 0 {
			b.WriteString(prompt.Tf(s.lang, "prompt.agenda.list.with", "，和 %s",
				strings.Join(c.With, prompt.T(s.lang, "prompt.agenda.render.name_sep", "、"))))
		}
		if c.Place != "" {
			b.WriteString(prompt.Tf(s.lang, "prompt.agenda.list.place", "，在 %s", c.Place))
		}
		if c.Note != "" {
			b.WriteString(prompt.Tf(s.lang, "prompt.agenda.list.note", "；%s", c.Note))
		}
		if c.Planned {
			b.WriteString(prompt.T(s.lang, "prompt.agenda.list.planned", "（已排进当天的表）"))
		}
		b.WriteString("\n")
	}
	if matched == 0 {
		return prompt.Tf(s.lang, "prompt.agenda.result.no_commit_that_day", "共 %d 条约定，%s 没有安排。",
			len(cs), fmtDateCN(s.lang, filter, now.Location())), nil
	}
	return prompt.Tf(s.lang, "prompt.agenda.result.list_count", "共 %d 条：", matched) + "\n" +
		strings.TrimRight(b.String(), "\n"), nil
}

// ---------- add_promise ----------

type addPromiseTool struct{ p *Plugin }

func (t *addPromiseTool) Name() string { return "add_promise" }

func (t *addPromiseTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.agenda.tool.add_promise.desc",
		"记下一件答应下来的事：你答应对方的，或者对方答应你的。"+
			"用于没有具体时刻、只落在某一天的事（明天给他带两个菜、周末把书还回去）；"+
			"定在某天某时的事用 add_commitment，要对方到点被提醒的事用定时任务。"+
			"在对话里说出口就记，别只在话里说过就算——说过的话不会自己变成待办。")
}

func (t *addPromiseTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.agenda.tool.add_promise.schema",
		addpromiseSchemaZH))
}

func (t *addPromiseTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Title string `json:"title"`
		Date  string `json:"date"`
		By    string `json:"by"`
		Note  string `json:"note"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}
	p := t.p
	s := p.snapshot()
	st := p.writeStore(ctx)
	if st == nil {
		return "", errNotReady(s.lang)
	}
	now := p.now()
	today := s.today(now).Format(dateLayout)

	pr := Promise{Title: squash(a.Title), Note: squash(a.Note),
		By: strings.TrimSpace(a.By), Status: promisePending, Created: now}
	if pr.Title == "" {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.agenda.result.title_empty", "title 不能为空"))
	}
	if err := checkRunes(s.lang, pr.Title, "title", maxTitleRunes); err != nil {
		return "", err
	}
	if err := checkRunes(s.lang, pr.Note, "note", maxNoteRunes); err != nil {
		return "", err
	}
	if pr.By != promiseBySelf && pr.By != promiseByUser {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.agenda.result.bad_by",
			"by 只能是 self（你答应的）或 user（对方答应的）"))
	}
	d, err := parseDate(s.lang, a.Date, now.Location())
	if err != nil {
		return "", err
	}
	pr.Date = d.Format(dateLayout)
	if pr.Date < today {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.promise_date_past",
			"%s 已经过去了，只能记今天或之后要兑现的事"), fmtDateCN(s.lang, pr.Date, now.Location()))
	}

	var full bool
	_, err = st.UpdatePromises(func(ps *[]Promise, next func() string) (bool, error) {
		if countPending(*ps) >= maxPromises {
			full = true
			return false, nil
		}
		pr.ID = next()
		*ps = append(*ps, pr)
		return true, nil
	})
	if err != nil {
		return "", err
	}
	if full {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.promise_limit",
			"还没了结的事已有 %d 条，先用 settle_promise 了结几条再记"), maxPromises)
	}
	return prompt.Tf(s.lang, "prompt.agenda.result.promise_added",
		"记下了 %s：%s %s（%s）。兑现或没做成时用 settle_promise 了结它。",
		pr.ID, promiseByCN(s.lang, pr.By), pr.Title, fmtDateCN(s.lang, pr.Date, now.Location())), nil
}

// ---------- settle_promise ----------

type settlePromiseTool struct{ p *Plugin }

func (t *settlePromiseTool) Name() string { return "settle_promise" }

func (t *settlePromiseTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.agenda.tool.settle_promise.desc",
		"了结一件答应过的事：做到了（done）、没做成（missed）、或者不作数了（dropped）。"+
			"了结之后它不再出现在 [答应过的事] 里，也就不会被再提一遍。")
}

func (t *settlePromiseTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.agenda.tool.settle_promise.schema",
		settlepromiseSchemaZH))
}

func (t *settlePromiseTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID      string `json:"id"`
		Result  string `json:"result"`
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}
	p := t.p
	s := p.snapshot()
	st := p.writeStore(ctx)
	if st == nil {
		return "", errNotReady(s.lang)
	}
	now := p.now()
	id := strings.TrimSpace(a.ID)
	outcome := squash(a.Outcome)
	if err := checkRunes(s.lang, outcome, "outcome", maxOutcomeRunes); err != nil {
		return "", err
	}
	switch a.Result {
	case promiseDone, promiseMissed, promiseDropped:
	default:
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.agenda.result.bad_result",
			"result 只能是 done / missed / dropped"))
	}

	var found, already bool
	var title string
	if _, err := st.UpdatePromises(func(ps *[]Promise, _ func() string) (bool, error) {
		for i := range *ps {
			pr := &(*ps)[i]
			if pr.ID != id {
				continue
			}
			found = true
			title = pr.Title
			if pr.settled() {
				already = true
				return false, nil
			}
			pr.Status, pr.Outcome, pr.Settled = a.Result, outcome, now
			return true, nil
		}
		return false, nil
	}); err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.agenda.result.no_promise", "没有编号为 %s 的事"), id)
	}
	if already {
		return prompt.Tf(s.lang, "prompt.agenda.result.promise_already",
			"%s（%s）早就了结过了，不必重复提交。", id, title), nil
	}
	return prompt.Tf(s.lang, "prompt.agenda.result.promise_settled", "%s（%s）已了结：%s。",
		id, title, promiseResultCN(s.lang, a.Result)), nil
}

// ---------- list_promises ----------

type listPromisesTool struct{ p *Plugin }

func (t *listPromisesTool) Name() string { return "list_promises" }

func (t *listPromisesTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.agenda.tool.list_promises.desc",
		"查看答应过的事，含已经了结的。[答应过的事] 只注入还没了结的那几条，"+
			"要回头看某件事后来做没做用它。")
}

func (t *listPromisesTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.agenda.tool.list_promises.schema",
		listpromisesSchemaZH))
}

func (t *listPromisesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		IncludeSettled bool `json:"include_settled"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &a)
	}
	p := t.p
	s := p.snapshot()
	now := p.now()
	var all []Promise
	// 按可读域汇总：藏起来的域不该经条数或标题漏出存在性
	for _, tag := range p.readDomains(ctx) {
		ps, err := p.storeFor(tag).LoadPromises()
		if err != nil {
			return "", err
		}
		all = append(all, ps...)
	}
	sortPromises(all)
	var b strings.Builder
	n := 0
	for _, pr := range all {
		if pr.settled() && !a.IncludeSettled {
			continue
		}
		n++
		fmt.Fprintf(&b, prompt.T(s.lang, "prompt.agenda.list.promise_line", "[%s] %s %s %s"),
			pr.ID, fmtDateCN(s.lang, pr.Date, now.Location()), promiseByCN(s.lang, pr.By), pr.Title)
		if pr.Note != "" {
			b.WriteString(prompt.Tf(s.lang, "prompt.agenda.render.promise_note", "（%s）", pr.Note))
		}
		if pr.settled() {
			b.WriteString(prompt.Tf(s.lang, "prompt.agenda.list.promise_result", " —— %s",
				promiseResultCN(s.lang, pr.Status)))
			if pr.Outcome != "" {
				b.WriteString(prompt.Tf(s.lang, "prompt.agenda.list.promise_outcome", "：%s", pr.Outcome))
			}
		}
		b.WriteString("\n")
	}
	if n == 0 {
		return prompt.T(s.lang, "prompt.agenda.result.no_promises", "没有答应过的事。"), nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// 八份参数 Schema。整段一个键：它们是手写的 JSON，拆成一个字段一个键的话，译者要
// 照着结构去拼，而拼错一个引号就是一份非法的 Schema。带 %[n]s 的位置由 prompt.Enum
// 按语言填出候选项。
const setdayplanSchemaZH = `{
		"type": "object",
		"properties": {
			"items": {"type": "array", "items": {"type": "object", "properties": {
				"title": {"type": "string", "description": "做什么，短句（30 字内）"},
				"start": {"type": "string", "description": "开始时间 HH:MM"},
				"end": {"type": "string", "description": "结束时间 HH:MM，早于开始时间表示跨到次日"},
				"place": {"type": "string", "description": "在哪（30 字内，可省）"},
				"with": {"type": "array", "items": {"type": "string"}, "description": "同行的人，必须是人物清单里已有的名字"},
				"with_user": {"type": "boolean", "description": "是否与对方一起；为真时这项一律不能动"},
				"flex": {"type": "string", "enum": %[1]s, "description": "能不能挪"},
				"busy": {"type": "string", "enum": %[2]s, "description": "做这件事时能不能回消息：轻忙可随时回，重忙只能简短回，不回是完全顾不上。不填为轻忙"},
				"from_commitment": {"type": "string", "description": "若这项来自某条今天的约定，填它的 id"}
			}, "required": ["title", "start", "end", "flex"]}},
			"note": {"type": "string", "description": "这一天的基调，一句话（可省，不注入）"}
		},
		"required": ["items"]
	}`

const updatedayplanSchemaZH = `{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "项的 id，如 a2"},
			"status": {"type": "string", "enum": ["ongoing", "done", "skipped", "deferred", "cancelled"], "description": "新状态：开始了 / 做完了 / 跳过 / 延期到别天 / 取消"},
			"start": {"type": "string", "description": "新的开始时间 HH:MM"},
			"end": {"type": "string", "description": "新的结束时间 HH:MM"},
			"title": {"type": "string", "description": "改了做法时更新标题（30 字内）"},
			"busy": {"type": "string", "enum": %[1]s, "description": "做这件事时能不能回消息"},
			"outcome": {"type": "string", "description": "做完时写一句经历；跳过、取消、延期时写原因（80 字内）"},
			"defer_to": {"type": "string", "description": "status 为 deferred 时必填：新日期 YYYY-MM-DD，可带 HH:MM"},
			"agreed_by_user": {"type": "boolean", "description": "与对方的约定要改时间、取消、延期或不去时，只在对方在本轮对话里明确同意后才传 true"}
		},
		"required": ["id"]
	}`

const addcommitmentSchemaZH = `{
		"type": "object",
		"properties": {
			"date": {"type": "string", "description": "日期 YYYY-MM-DD，不能是今天以前"},
			"start": {"type": "string", "description": "开始时间 HH:MM"},
			"end": {"type": "string", "description": "结束时间 HH:MM（可省）"},
			"title": {"type": "string", "description": "做什么，短句（30 字内）"},
			"with": {"type": "array", "items": {"type": "string"}, "description": "同行的人，必须是人物清单里已有的名字"},
			"with_user": {"type": "boolean", "description": "是否与对方一起；为真时一律不能动"},
			"place": {"type": "string", "description": "在哪（30 字内，可省）"},
			"note": {"type": "string", "description": "备注一句（60 字内，可省）"},
			"flex": {"type": "string", "enum": %[1]s, "description": "能不能挪，不填为尽量守"}
		},
		"required": ["date", "start", "title"]
	}`

const cancelcommitmentSchemaZH = `{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "约定的 id，如 c8"},
			"reason": {"type": "string", "description": "取消的原因，一句话（80 字内）"},
			"agreed_by_user": {"type": "boolean", "description": "与对方的约定，只在对方在本轮对话里明确同意后才传 true"}
		},
		"required": ["id", "reason"]
	}`

const listcommitmentsSchemaZH = `{
		"type": "object",
		"properties": {
			"date": {"type": "string", "description": "只看这一天 YYYY-MM-DD；留空表示全部"}
		}
	}`

const addpromiseSchemaZH = `{
		"type": "object",
		"properties": {
			"title": {"type": "string", "description": "答应了什么，短句（30 字内）"},
			"date": {"type": "string", "description": "打算在哪天兑现 YYYY-MM-DD，不能是今天以前；说「明天」就填明天的日期"},
			"by": {"type": "string", "enum": ["self", "user"], "description": "谁答应的：self 你自己，user 对方"},
			"note": {"type": "string", "description": "补一句细节（60 字内，可省）"}
		},
		"required": ["title", "date", "by"]
	}`

const settlepromiseSchemaZH = `{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "[答应过的事] 里那条的编号，如 p1"},
			"result": {"type": "string", "enum": ["done", "missed", "dropped"], "description": "做到了 / 没做成 / 不作数了"},
			"outcome": {"type": "string", "description": "一句话说清结果或原因（80 字内）"}
		},
		"required": ["id", "result"]
	}`

const listpromisesSchemaZH = `{
		"type": "object",
		"properties": {
			"include_settled": {"type": "boolean", "description": "是否连已了结的一并列出，默认只列还没了结的"}
		}
	}`
