package stylewatch

import (
	"context"
	"fmt"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// 设置页上的两个操作：看 30 天的报告、清空统计。
//
// 报告作为操作而不是只靠工具：工具要经模型之手才能调，而看数字是人的事。
// 清空给换角色或改完提示词之后用——旧角色的命中混在新角色的统计里，趋势就看不出了。
const (
	actionReport = "report"
	actionClear  = "clear"
)

func (p *Plugin) Actions() []plugin.ActionDef {
	return []plugin.ActionDef{
		{
			Key:         actionReport,
			Label:       "查看 30 天报告",
			Description: "每天一行：轮数、助手腔命中、平均字数、【】演绎占比。",
		},
		{
			Key:         actionClear,
			Label:       "清空统计",
			Description: "删掉全部按天的统计。换了角色或改了提示词之后用，不可撤销。",
		},
	}
}

func (p *Plugin) StartAction(_ context.Context, key string) error {
	switch key {
	case actionReport:
		p.setAction(key, plugin.ActionState{Status: plugin.ActionDone, Message: p.renderReport(p.lang(), keepDays)})
		return nil
	case actionClear:
		s := p.snapshot()
		if s.dir == "" {
			return fmt.Errorf("%s", i18n.T(p.lang(), "plugin.style_watch.not_ready", "文风观察尚未就绪"))
		}
		p.statsMu.Lock()
		p.st = stats{}
		p.seq++
		snap, seq := p.st.clone(), p.seq
		p.statsMu.Unlock()
		p.persist(s.dir, snap, seq)
		p.setAction(key, plugin.ActionState{Status: plugin.ActionDone, Message: i18n.T(p.lang(), "plugin.style_watch.cleared", "已清空全部统计。")})
		return nil
	}
	return plugin.UnknownAction(p.lang(), key)
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if key != actionReport && key != actionClear {
		return plugin.ActionState{}, plugin.UnknownAction(p.lang(), key)
	}
	p.actMu.Lock()
	defer p.actMu.Unlock()
	st, ok := p.actStates[key]
	if !ok {
		return plugin.ActionState{Status: plugin.ActionIdle}, nil
	}
	return st, nil
}

func (p *Plugin) setAction(key string, st plugin.ActionState) {
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if p.actStates == nil {
		p.actStates = map[string]plugin.ActionState{}
	}
	p.actStates[key] = st
}
