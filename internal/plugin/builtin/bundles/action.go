package bundles

import (
	"context"
	"fmt"
	"strings"
	"time"

	"wen/internal/agentplugin"
	"wen/internal/i18n"
	"wen/internal/plugin"
)

const (
	actionInstall = "install"
	actionRefresh = "refresh"
)

// 握手整体的时限：几台加起来别把界面挂在那儿。单台的连接超时另有一份。
const handshakeTimeout = 120 * time.Second

func (p *Plugin) Actions() []plugin.ActionDef {
	return []plugin.ActionDef{
		{
			Key:   actionInstall,
			Label: "安装",
			Description: "装上面那一栏填的插件包。装完会连一次它声明的 MCP server 取工具清单——" +
				"那一步会在本机执行插件包里写的命令。",
			Detail: p.installedSummary(),
		},
		{
			Key:   actionRefresh,
			Label: "重新握手全部",
			Description: "把已装的每个插件包都连一遍，工具清单重取。包更新了、依赖后来才装好、" +
				"上次没连上时用它。",
			Detail: p.installedSummary(),
		},
	}
}

// StartAction 立即返回，长流程放后台。
func (p *Plugin) StartAction(ctx context.Context, key string) error {
	lang := p.langOf()
	switch key {
	case actionInstall:
		// 草稿值必须在这里同步取出：ctx 属于那个 HTTP 请求，响应发出后就失效了，
		// 而安装在后台 goroutine 里跑。这样用户填完路径不必先保存就能装。
		src := strings.TrimSpace(plugin.ActionValueOr(plugin.ActionValuesFrom(ctx), "source", ""))
		if src == "" {
			err := fmt.Errorf("%s", i18n.T(lang, "plugin.bundles.err.no_source",
				"先在上面那一栏填上插件包的路径"))
			p.finish(p.begin(plugin.ActionState{}), plugin.ActionState{
				Status: plugin.ActionError, Message: err.Error() + "。",
			})
			return err
		}
		seq := p.begin(plugin.ActionState{Status: plugin.ActionPending,
			Message: i18n.Tf(lang, "plugin.bundles.step.installing", "正在安装 %s …", src)})
		go p.runInstall(seq, lang, src)
		return nil
	case actionRefresh:
		seq := p.begin(plugin.ActionState{Status: plugin.ActionPending,
			Message: i18n.T(lang, "plugin.bundles.step.connecting", "正在连接已装的插件包 …")})
		go p.runRefresh(seq, lang)
		return nil
	}
	return plugin.UnknownAction(lang, key)
}

func (p *Plugin) runInstall(seq uint64, lang, src string) {
	b, err := p.store.Install(src)
	if err != nil {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
		return
	}

	var sb strings.Builder
	sb.WriteString(i18n.Tf(lang, "plugin.bundles.install.done", "装好了 %s", b.Manifest.Name))
	if b.Manifest.Version != "" {
		sb.WriteString(i18n.Tf(lang, "plugin.bundles.install.version", "（%s）", b.Manifest.Version))
	}
	sb.WriteString(i18n.Tf(lang, "plugin.bundles.install.where", "\n格式：%s\n位置：%s\n",
		b.Format, p.store.Path(b.Manifest.Name)))
	if b.SkillsDir != "" {
		sb.WriteString(i18n.T(lang, "plugin.bundles.install.skills",
			"带技能：要「技能手册」插件开着才会进清单。\n"))
	}
	for _, note := range b.Problems {
		sb.WriteString(i18n.Tf(lang, "plugin.bundles.install.note", "提示：%s\n", note))
	}

	if len(b.Servers) > 0 {
		tools, problems := p.handshake(b)
		sb.WriteString(i18n.Tf(lang, "plugin.bundles.install.tools", "\n取到 %d 件工具", len(tools)))
		if len(tools) > 0 {
			sb.WriteString(i18n.T(lang, "plugin.bundles.install.tools_colon", "：\n"))
			for _, t := range tools {
				fmt.Fprintf(&sb, "· %s\n", t.Local)
			}
		} else {
			sb.WriteString(i18n.T(lang, "plugin.bundles.install.tools_end", "。\n"))
		}
		for _, m := range problems {
			fmt.Fprintf(&sb, "× %s\n", m)
		}
	}

	// 说清「装好了」与「用得上了」不是一回事：插件在启动期注册，这一轮的插件列表
	// 不会变。不说的话，用户会盯着没有新卡片的页面以为装失败了。
	sb.WriteString(i18n.T(lang, "plugin.bundles.install.restart_note",
		"\n重启服务之后它才会出现在插件列表里。"))
	p.finish(seq, plugin.ActionState{Status: plugin.ActionDone, Message: strings.TrimRight(sb.String(), "\n")})
}

func (p *Plugin) runRefresh(seq uint64, lang string) {
	list, _ := p.store.List()
	if len(list) == 0 {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionDone,
			Message: i18n.T(lang, "plugin.bundles.refresh.none", "一个插件包都没装。")})
		return
	}

	var sb strings.Builder
	var total int
	for _, b := range list {
		if len(b.Servers) == 0 {
			continue
		}
		tools, problems := p.handshake(b)
		total += len(tools)
		sb.WriteString(i18n.Tf(lang, "plugin.bundles.refresh.entry", "%s：%d 件工具\n",
			b.Manifest.Name, len(tools)))
		for _, m := range problems {
			fmt.Fprintf(&sb, "· × %s\n", m)
		}
	}
	if sb.Len() == 0 {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionDone,
			Message: i18n.T(lang, "plugin.bundles.refresh.no_mcp",
				"已装的插件包里没有 MCP 声明，没什么可握手的。")})
		return
	}
	sb.WriteString(i18n.Tf(lang, "plugin.bundles.refresh.total",
		"\n共 %d 件工具。新增的工具要重启之后才可用。", total))
	p.finish(seq, plugin.ActionState{Status: plugin.ActionDone, Message: strings.TrimRight(sb.String(), "\n")})
}

// handshake 连一个包并把工具清单存下来。
func (p *Plugin) handshake(b *agentplugin.Bundle) ([]agentplugin.ToolInfo, []string) {
	lang := p.langOf()
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	tools, problems, err := agentplugin.Handshake(ctx, lang, b, agentplugin.DataDir(p.baseDir, b.Manifest.Name))
	if err != nil {
		return nil, append(problems, err.Error())
	}
	if err := p.store.SaveTools(b.Manifest.Name, tools); err != nil {
		return tools, append(problems, i18n.Tf(lang, "plugin.bundles.err.save_tools",
			"工具清单没能存下来：%s", err.Error()))
	}
	return tools, problems
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if key != actionInstall && key != actionRefresh {
		return plugin.ActionState{}, plugin.UnknownAction(p.langOf(), key)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.actState, nil
}

// begin 开一次新操作。两个操作共用一份状态：它们都是「装/连插件包」这一件事的两个
// 入口，同时跑没有意义，后触发的那次覆盖掉前一次的显示正是想要的。
func (p *Plugin) begin(st plugin.ActionState) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actSeq++
	p.actState = st
	return p.actSeq
}

func (p *Plugin) finish(seq uint64, st plugin.ActionState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.actSeq == seq {
		p.actState = st
	}
}
