package health

import (
	"context"
	"fmt"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// actionClear 是设置页上的「清除全部身体状况」按钮。
//
// 模型自己能标记痊愈，病程也会自己走完，所以这个按钮不是必需的——它管的是另一种
// 情况：换了角色设定，旧角色的病还挂在身上；或者冷却期把想要的剧情挡住了。
// 这两种都该由人来判断，不给模型。连同「上次痊愈」一起清掉，冷却随之解除。
const actionClear = "clear"

func (p *Plugin) Actions() []plugin.ActionDef {
	return []plugin.ActionDef{{
		Key:         actionClear,
		Label:       "清除全部身体状况",
		Description: "抹掉记下的全部状况与痊愈记录（冷却随之解除），包括各人格分开保存的那几份。不可撤销。",
	}}
}

// StartAction 清除全部可见域的记录。用户拥有全部域，这里不做可见域过滤——
// 界面上的操作不是模型的读取路径，不存在泄漏问题。
func (p *Plugin) StartAction(_ context.Context, key string) error {
	if key != actionClear {
		return plugin.UnknownAction(p.langOf(), key)
	}
	base, tags := p.allDomains()
	if base == "" {
		return errNotReady(p.langOf())
	}

	var failed []string
	cleared := 0
	for _, tag := range tags {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		ids, err := store.Clear()
		if err != nil {
			failed = append(failed, err.Error())
			continue
		}
		for _, id := range ids {
			dropCue(tag, id) // 还没说出口的「开始发作了」随记录一起作废
		}
		cleared++
	}

	p.actMu.Lock()
	defer p.actMu.Unlock()
	if len(failed) > 0 {
		msg := i18n.Tf(p.langOf(), "plugin.health.clear.failed", "清除身体状况失败：%s",
			strings.Join(failed, "; "))
		p.actState = plugin.ActionState{Status: plugin.ActionError, Message: msg}
		return fmt.Errorf("%s", msg)
	}
	p.actState = plugin.ActionState{
		Status:  plugin.ActionDone,
		Message: i18n.Tf(p.langOf(), "plugin.health.clear.done", "已清除 %d 份身体状况记录。", cleared),
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
