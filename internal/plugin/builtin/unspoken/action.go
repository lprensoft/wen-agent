package unspoken

import (
	"context"
	"fmt"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// actionClear 是设置页上的「清空心里话」按钮。
//
// 模型能逐条放下，但「这些全都不作数了」是换角色设定、换故事线时人的判断——
// 这件事不给模型做，工具层不该有入口。
const actionClear = "clear"

func (p *Plugin) Actions() []plugin.ActionDef {
	return []plugin.ActionDef{{
		Key:         actionClear,
		Label:       "清空心里话",
		Description: "抹掉全部心里话，包括各人格分开保存的那几份。不可撤销。",
	}}
}

// StartAction 清空全部可见域的清单。用户拥有全部域，这里不做可见域过滤——
// 界面上的操作不是模型的读取路径，不存在泄漏问题。
func (p *Plugin) StartAction(_ context.Context, key string) error {
	if key != actionClear {
		return plugin.UnknownAction(p.langOf(), key)
	}
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return errNotReady(p.langOf())
	}

	// Scope 取零值：ReadDomains 因此枚举磁盘上已存在的全部域，正好是「全部清空」的范围。
	var failed []string
	cleared := 0
	for _, tag := range plugin.ReadDomains(base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		if err := store.Clear(); err != nil {
			failed = append(failed, err.Error())
			continue
		}
		cleared++
	}

	p.actMu.Lock()
	defer p.actMu.Unlock()
	if len(failed) > 0 {
		msg := i18n.Tf(p.langOf(), "plugin.unspoken.clear.failed",
			"清空心里话失败：%s", strings.Join(failed, "; "))
		p.actState = plugin.ActionState{Status: plugin.ActionError, Message: msg}
		return fmt.Errorf("%s", msg)
	}
	p.actState = plugin.ActionState{
		Status:  plugin.ActionDone,
		Message: i18n.Tf(p.langOf(), "plugin.unspoken.clear.done", "已清空 %d 份心里话。", cleared),
	}
	return nil
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if key != actionClear {
		return plugin.ActionState{}, plugin.UnknownAction(p.langOf(), key)
	}
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if p.actState.Status == "" {
		return plugin.ActionState{Status: plugin.ActionIdle}, nil
	}
	return p.actState, nil
}
