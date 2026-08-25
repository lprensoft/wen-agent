package agentplugin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wen/internal/plugin"
)

// actionRefresh 是设置页上的「重新握手」按钮。
//
// 工具清单来自安装时落的缓存，所以它会过期：插件包自己更新了、server 依赖装好了、
// 上次握手时对方没起来。这个按钮把清单重取一遍。新工具要下次重启才进得了模型的
// 工具表（插件在启动期注册），按钮的结果里会说明这一点。
const actionRefresh = "refresh"

// 握手整体的时限。单台 server 的连接超时另有一份（见 client），这里管的是「几台
// 加起来别把界面挂在那儿」。
const handshakeTimeout = 90 * time.Second

// actionState 是操作的进展，与主状态分开加锁：StartAction 与 ActionState 由 HTTP 并发调用。
type actionState struct {
	mu    sync.Mutex
	seq   uint64
	state plugin.ActionState
}

func (p *Plugin) Actions() []plugin.ActionDef {
	if len(p.bundle.Servers) == 0 {
		return nil // 没有 MCP 组件就没什么可握手的
	}
	desc := fmt.Sprintf("连一遍这个包里的 %d 台 MCP server，把它们提供的工具重新记下来。",
		len(p.bundle.Servers))
	if len(p.tools) == 0 {
		desc += "当前还没有记下任何工具——装好之后没握过手，或者上次握手没成功。"
	} else {
		desc += fmt.Sprintf("当前记着 %d 件工具。", len(p.tools))
	}
	return []plugin.ActionDef{{Key: actionRefresh, Label: "重新握手", Description: desc}}
}

// StartAction 立即返回，握手在后台进行。
func (p *Plugin) StartAction(_ context.Context, key string) error {
	if key != actionRefresh {
		return fmt.Errorf("未知的操作 %q", key)
	}
	p.mu.RLock()
	dir := p.stateDir
	p.mu.RUnlock()
	if dir == "" {
		err := fmt.Errorf("这个插件还没初始化，先启用它再握手")
		p.finishAction(p.beginAction(plugin.ActionState{}), plugin.ActionState{
			Status: plugin.ActionError, Message: err.Error() + "。",
		})
		return err
	}

	seq := p.beginAction(plugin.ActionState{
		Status:  plugin.ActionPending,
		Message: fmt.Sprintf("正在连接 %d 台 server …", len(p.bundle.Servers)),
	})
	go p.runRefresh(seq, filepath.Join(dir, DataDirName))
	return nil
}

func (p *Plugin) runRefresh(seq uint64, dataDir string) {
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	tools, problems, err := Handshake(ctx, p.bundle, dataDir)
	if err != nil {
		p.finishAction(seq, plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
		return
	}
	if p.store != nil {
		if err := p.store.SaveTools(p.bundle.Manifest.Name, tools); err != nil {
			p.finishAction(seq, plugin.ActionState{
				Status:  plugin.ActionError,
				Message: "工具清单没能存下来：" + err.Error(),
			})
			return
		}
	}

	var b strings.Builder
	if len(tools) == 0 {
		b.WriteString("一件工具都没取到。\n")
	} else {
		fmt.Fprintf(&b, "取到 %d 件工具：\n", len(tools))
		for _, t := range tools {
			fmt.Fprintf(&b, "· %s\n", t.Local)
		}
	}
	if len(problems) > 0 {
		fmt.Fprintf(&b, "\n有 %d 台没能连上：\n", len(problems))
		for _, m := range problems {
			fmt.Fprintf(&b, "· %s\n", m)
		}
	}
	// 说清「记下来了」与「用得上了」不是一回事：插件在启动期注册，工具表这一轮不会变
	b.WriteString("\n清单已存下。工具表在服务启动时定下，重启之后这些工具才会真的可用。")

	st := plugin.ActionState{Status: plugin.ActionDone, Message: strings.TrimRight(b.String(), "\n")}
	if len(tools) == 0 && len(problems) > 0 {
		st.Status = plugin.ActionError
	}
	p.finishAction(seq, st)
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if key != actionRefresh {
		return plugin.ActionState{}, fmt.Errorf("未知的操作 %q", key)
	}
	p.act.mu.Lock()
	defer p.act.mu.Unlock()
	return p.act.state, nil
}

// beginAction 开一次新的操作，返回序号。进行中重复触发＝重新开始，旧的那次结束时
// 会因为序号对不上而不写状态。
func (p *Plugin) beginAction(st plugin.ActionState) uint64 {
	p.act.mu.Lock()
	defer p.act.mu.Unlock()
	p.act.seq++
	p.act.state = st
	return p.act.seq
}

func (p *Plugin) finishAction(seq uint64, st plugin.ActionState) {
	p.act.mu.Lock()
	defer p.act.mu.Unlock()
	if p.act.seq == seq {
		p.act.state = st
	}
}
