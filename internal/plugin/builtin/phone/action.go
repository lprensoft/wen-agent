package phone

import (
	"context"
	"net/http"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// actionTest 是设置页上的「试刷订阅源」：逐个取一遍，报告每个源叫什么、有几条、最新
// 一条是什么，或者坏在哪儿。不改任何状态——试的可能是还没保存的草稿，也不该因为点了
// 一下按钮就让她「刷过了」。
const actionTest = "test"

func (p *Plugin) Actions() []plugin.ActionDef {
	return []plugin.ActionDef{{
		Key:         actionTest,
		Label:       "试刷订阅源",
		Description: "逐个取一遍上面填的订阅源（含尚未保存的修改），看看地址对不对、能读到什么。不会算作她刷过手机。",
	}}
}

func (p *Plugin) StartAction(ctx context.Context, key string) error {
	lang := p.langOf()
	if key != actionTest {
		return plugin.UnknownAction(lang, key)
	}
	// 草稿值在这里同步取出：ctx 属于那次 HTTP 请求，后台 goroutine 里就失效了
	s := p.snapshot()
	raw := plugin.ActionValueOr(plugin.ActionValuesFrom(ctx), "feeds", strings.Join(s.feeds, "\n"))
	feeds, err := parseFeeds(lang, raw)
	if err != nil {
		p.setAction(0, plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
		return err
	}
	if len(feeds) == 0 {
		st := plugin.ActionState{Status: plugin.ActionError,
			Message: i18n.T(lang, "plugin.phone.test.none", "还没有填订阅源。")}
		p.setAction(0, st)
		return nil
	}
	p.actMu.Lock()
	p.actSeq++
	seq := p.actSeq
	p.actState = plugin.ActionState{Status: plugin.ActionPending,
		Message: i18n.Tf(lang, "plugin.phone.test.running", "正在取 %d 个订阅源…", len(feeds))}
	p.actMu.Unlock()
	go p.runTest(seq, lang, feeds)
	return nil
}

func (p *Plugin) runTest(seq uint64, lang string, feeds []string) {
	client := &http.Client{Timeout: fetchTimeout}
	var b strings.Builder
	failed := 0
	for _, u := range feeds {
		name, entries, err := fetchFeed(context.Background(), client, u)
		switch {
		case err != nil:
			failed++
			b.WriteString(i18n.Tf(lang, "plugin.phone.test.failed", "· %s\n  取不到：%v\n", u, err))
		case len(entries) == 0:
			b.WriteString(i18n.Tf(lang, "plugin.phone.test.empty", "· %s\n  「%s」，里面没有条目\n", u, name))
		default:
			b.WriteString(i18n.Tf(lang, "plugin.phone.test.ok", "· %s\n  「%s」，%d 条，最新一条：%s\n",
				u, name, len(entries), entries[0].Title))
		}
	}
	st := plugin.ActionState{Status: plugin.ActionDone, Message: strings.TrimRight(b.String(), "\n")}
	if failed == len(feeds) {
		st.Status = plugin.ActionError
	}
	p.setAction(seq, st)
}

// setAction 写入操作状态。seq 非零时只认最新那一次：连点两下，先跑完的旧结果不该
// 盖掉后一次的进展。
func (p *Plugin) setAction(seq uint64, st plugin.ActionState) {
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if seq != 0 && seq != p.actSeq {
		return
	}
	if seq == 0 {
		p.actSeq++
	}
	p.actState = st
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
