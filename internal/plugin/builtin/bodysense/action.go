package bodysense

import (
	"context"
	"fmt"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// actionClear 是设置页上的「清空接触记录」按钮。
//
// 这件事不给模型做：角色设定一改，计数还挂在旧角色身上，而系统里没有「角色身份」
// 这个概念，只能由人来判断该不该清。这是不可撤销的破坏性操作，工具层不该有入口。
const (
	actionClear      = "clear"
	actionResetState = "reset_state"
)

func (p *Plugin) Actions() []plugin.ActionDef {
	return []plugin.ActionDef{
		{
			Key:         actionClear,
			Label:       "清空接触记录",
			Description: "把全部部位的累计次数清零，包括各人格分开保存的那几份。不可撤销。",
		},
		{
			Key:         actionResetState,
			Label:       "重置身体状态",
			Description: "把唤起与疲劳清回 0，包括各人格分开保存的那几份。不可撤销。",
		},
	}
}

// StartAction 清空全部可见域的记录或状态。用户拥有全部域，这里不做可见域过滤——
// 界面上的操作不是模型的读取路径，不存在泄漏问题。
func (p *Plugin) StartAction(_ context.Context, key string) error {
	// 两句回执各自成句、键与原文都写在调用点上。把「清空 %s 失败」里的名词抽出来当
	// 变量看着更省，但那个名词得跟着界面语言变，拼出来的句子在另一种语言里未必还是
	// 这个语序；把键抽成变量也不行，覆盖检查是静态扫 (键, 原文) 对的，扫不到就等于
	// 这两句永远不会被提醒漏译。
	var (
		clearOne func(tag string) error
		done     func(lang string, n int) string
		failed1  func(lang, detail string) string
	)
	switch key {
	case actionClear:
		done = func(lang string, n int) string {
			return i18n.Tf(lang, "plugin.body_sense.clear.done", "已清空 %d 份接触记录。", n)
		}
		failed1 = func(lang, detail string) string {
			return i18n.Tf(lang, "plugin.body_sense.clear.failed", "清空接触记录失败：%s", detail)
		}
		clearOne = func(tag string) error {
			store := p.storeFor(tag)
			if store == nil {
				return nil
			}
			return store.Clear()
		}
	case actionResetState:
		done = func(lang string, n int) string {
			return i18n.Tf(lang, "plugin.body_sense.reset_state.done", "已重置 %d 份身体状态。", n)
		}
		failed1 = func(lang, detail string) string {
			return i18n.Tf(lang, "plugin.body_sense.reset_state.failed", "重置身体状态失败：%s", detail)
		}
		clearOne = func(tag string) error {
			store := p.stateStoreFor(tag)
			if store == nil {
				return nil
			}
			return store.Clear()
		}
	default:
		return plugin.UnknownAction(p.langOf(), key)
	}
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return errNotReady(p.langOf())
	}

	// Scope 取零值：Write 为空、Read 为 nil，ReadDomains 因此枚举磁盘上已存在的
	// 全部域，正好是「清空所有」需要的范围。
	var failed []string
	cleared := 0
	for _, tag := range plugin.ReadDomains(base, plugin.Scope{}) {
		if err := clearOne(tag); err != nil {
			failed = append(failed, err.Error())
			continue
		}
		cleared++
	}

	p.actMu.Lock()
	defer p.actMu.Unlock()
	lang := p.langOf()
	if len(failed) > 0 {
		msg := failed1(lang, strings.Join(failed, "; "))
		p.actState = plugin.ActionState{Status: plugin.ActionError, Message: msg}
		return fmt.Errorf("%s", msg)
	}
	p.actState = plugin.ActionState{Status: plugin.ActionDone, Message: done(lang, cleared)}
	return nil
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if key != actionClear && key != actionResetState {
		return plugin.ActionState{}, plugin.UnknownAction(p.langOf(), key)
	}
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if p.actState.Status == "" {
		return plugin.ActionState{Status: plugin.ActionIdle}, nil
	}
	return p.actState, nil
}
