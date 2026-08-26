package weather

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// actionTest 是设置页上的「测试」按钮。
//
// 它测的是「这些城市名能不能被解析成一个地方」——这件事光看配置项看不出来：
// 地名写法五花八门，重名的地方也多，解析结果只有查一次才知道。所以它读的是
// 配置弹窗里**尚未保存**的草稿值，先验后存，而不是让人先保存一个错的再来发现。
const actionTest = "test"

// target 是本次要测的一处地点。
type target struct {
	label string // 「角色与我同在」「角色所在」「我所在」
	loc   string
}

func (p *Plugin) Actions() []plugin.ActionDef {
	return []plugin.ActionDef{{
		Key:         actionTest,
		Label:       "测试这些城市",
		Description: "用上面填写的城市各查一次天气，看看解析成了哪个地方、接口通不通。不改动已保存的配置。",
	}}
}

// StartAction 立即返回，查询在后台进行。
func (p *Plugin) StartAction(ctx context.Context, key string) error {
	if key != actionTest {
		return plugin.UnknownAction(p.langOf(), key)
	}
	s := p.snapshot()
	// 草稿值必须在这里同步取出：ctx 属于那个 HTTP 请求，响应发出后就失效了，
	// 而查询是在后台 goroutine 里跑的。
	targets := draftTargets(s.lang, plugin.ActionValuesFrom(ctx), s)
	if len(targets) == 0 {
		err := fmt.Errorf("%s", i18n.T(s.lang, "plugin.weather.action.no_city", "请先填写城市再测试"))
		p.setActionState(plugin.ActionState{Status: plugin.ActionError, Message: err.Error() + "。"})
		return err
	}
	client := s.client
	if client == nil {
		// 插件启用但一个城市都没填时不会建 client；测试不该因此用不了
		client = &http.Client{Timeout: requestTimeout}
	}

	seq := p.beginAction(plugin.ActionState{
		Status: plugin.ActionPending,
		Message: i18n.Tf(s.lang, "plugin.weather.action.querying", "正在查询%s的天气…",
			strings.Join(locNames(targets), i18n.T(s.lang, "plugin.weather.action.loc_sep", "、"))),
	})
	go p.runTest(seq, s.lang, client, targets)
	return nil
}

// draftTargets 把草稿值叠加到已保存的配置上，得出本次要测的地点。
// 归一化与 Init 保持一致：同城只测一处，两处填了同一个地方也只测一处。
func draftTargets(lang string, vals map[string]any, s settings) []target {
	personaLoc := plugin.ActionValueOr(vals, "persona_location", s.personaLoc)
	sameCity := s.sameCity
	if _, ok := vals["same_city"]; ok {
		sameCity = plugin.CfgBool(vals, "same_city", s.sameCity)
	}
	userLoc := ""
	if !sameCity {
		userLoc = plugin.ActionValueOr(vals, "user_location", s.userLoc)
	}
	if userLoc != "" && userLoc == personaLoc {
		sameCity, userLoc = true, ""
	}

	var out []target
	switch {
	case sameCity && personaLoc != "":
		out = append(out, target{label: i18n.T(lang, "plugin.weather.target.both", "角色与我同在"), loc: personaLoc})
	default:
		if personaLoc != "" {
			out = append(out, target{label: i18n.T(lang, "plugin.weather.target.persona", "角色所在"), loc: personaLoc})
		}
		if userLoc != "" {
			out = append(out, target{label: i18n.T(lang, "plugin.weather.target.user", "我所在"), loc: userLoc})
		}
	}
	return out
}

func locNames(targets []target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, "「"+t.loc+"」")
	}
	return out
}

// runTest 在后台逐个查一遍并写回结果。ctx 自建：请求的 ctx 到这里已经作废。
//
// 一处失败不跳过其余的：两处都填了的时候，一次点击就该把两处的结论都给出来，
// 而不是让人修好一个再发现另一个也不行。
func (p *Plugin) runTest(seq uint64, lang string, client *http.Client, targets []target) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout*time.Duration(len(targets)))
	defer cancel()

	var (
		b      strings.Builder
		failed int
	)
	for i, t := range targets {
		if i > 0 {
			b.WriteString("\n")
		}
		place, rep, err := probe(ctx, lang, client, t.loc)
		if err != nil {
			failed++
			b.WriteString(i18n.Tf(lang, "plugin.weather.report.failed",
				"%s（%s）：查询失败——%s\n", t.label, t.loc, err.Error()))
			continue
		}
		b.WriteString(i18n.Tf(lang, "plugin.weather.report.place",
			"%s：%s（纬度 %.4f，经度 %.4f）\n", t.label, place.Name, place.Lat, place.Lon))
		b.WriteString(i18n.Tf(lang, "plugin.weather.report.now", "天气：%s\n", renderConditions(lang, rep)))
	}

	st := plugin.ActionState{Status: plugin.ActionDone}
	if failed > 0 {
		st.Status = plugin.ActionError
	} else if len(targets) > 1 {
		b.WriteString(i18n.T(lang, "plugin.weather.report.both_ok",
			"\n两处都可用。保存后会按刷新间隔自动取用。"))
	} else {
		b.WriteString(i18n.T(lang, "plugin.weather.report.one_ok",
			"\n这个城市可用。保存后会按刷新间隔自动取用。"))
	}
	st.Message = strings.TrimRight(b.String(), "\n")
	p.finishAction(seq, st)
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if key != actionTest {
		return plugin.ActionState{}, plugin.UnknownAction(p.langOf(), key)
	}
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if p.actState.Status == "" {
		return plugin.ActionState{Status: plugin.ActionIdle}, nil
	}
	return p.actState, nil
}

// beginAction 记下新一次测试的状态并返回它的序号。
//
// 序号是为「进行中重复触发 = 重新开始」准备的：上一次查询还挂在网络上，用户改了
// 城市又点了一次，先发的那次可能后回来，没有序号的话它会把新结果覆盖掉。
func (p *Plugin) beginAction(st plugin.ActionState) uint64 {
	p.actMu.Lock()
	defer p.actMu.Unlock()
	p.actSeq++
	p.actState = st
	return p.actSeq
}

// finishAction 只在这一次仍是最新的那次时写回结果。
func (p *Plugin) finishAction(seq uint64, st plugin.ActionState) {
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if seq != p.actSeq {
		return
	}
	p.actState = st
}

func (p *Plugin) setActionState(st plugin.ActionState) {
	p.actMu.Lock()
	defer p.actMu.Unlock()
	p.actSeq++
	p.actState = st
}
