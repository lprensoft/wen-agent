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
	actionTrack   = "track"
	actionUntrack = "untrack"
)

const handshakeTimeout = 120 * time.Second

func knownAction(key string) bool {
	switch key {
	case actionInstall, actionRefresh, actionTrack, actionUntrack:
		return true
	}
	return false
}

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
		{
			Key:   actionTrack,
			Label: "绑定更新源",
			Description: "把包名、HTTPS 更新清单与发布者 Ed25519 公钥绑在一起。绑定只认证之后的更新，" +
				"不反向证明此前手工安装的包来自这个发布者。",
			Detail: p.trackedSummary(),
		},
		{
			Key:         actionUntrack,
			Label:       "解除绑定",
			Description: "删除这个包的更新来源与候选；有待重启应用的更新时必须先取消。不会卸载插件包。",
			Detail:      p.trackedSummary(),
		},
	}
}

func (p *Plugin) StartAction(ctx context.Context, key string) error {
	lang := p.langOf()
	values := plugin.ActionValuesFrom(ctx)
	var first, second, third string
	switch key {
	case actionInstall:
		first = strings.TrimSpace(plugin.ActionValueOr(values, "source", p.savedString("source")))
		if first == "" {
			return p.fail(key, fmt.Errorf("%s", i18n.T(lang, "plugin.bundles.err.no_source", "先在上面那一栏填上插件包的路径")))
		}
	case actionTrack:
		first = strings.TrimSpace(plugin.ActionValueOr(values, "update_name", p.savedString("update_name")))
		second = strings.TrimSpace(plugin.ActionValueOr(values, "update_url", p.savedString("update_url")))
		third = strings.TrimSpace(plugin.ActionValueOr(values, "update_key", p.savedString("update_key")))
		if first == "" || second == "" || third == "" {
			return p.fail(key, fmt.Errorf("%s", i18n.T(lang, "plugin.bundles.err.track_fields", "包名、更新清单 URL 与发布者公钥都要填写")))
		}
	case actionUntrack:
		first = strings.TrimSpace(plugin.ActionValueOr(values, "update_name", p.savedString("update_name")))
		if first == "" {
			return p.fail(key, fmt.Errorf("%s", i18n.T(lang, "plugin.bundles.err.update_name", "先填要解除绑定的插件包名称")))
		}
	case actionRefresh:
	default:
		return plugin.UnknownAction(lang, key)
	}

	pendingMessage := i18n.T(lang, "plugin.bundles.step.working", "操作进行中 …")
	if key == actionInstall {
		pendingMessage = i18n.Tf(lang, "plugin.bundles.step.installing", "正在安装 %s …", first)
	} else if key == actionRefresh {
		pendingMessage = i18n.T(lang, "plugin.bundles.step.connecting", "正在连接已装的插件包 …")
	}
	seq, err := p.begin(plugin.ActionState{Status: plugin.ActionPending, Message: pendingMessage})
	if err != nil {
		return err
	}
	switch key {
	case actionInstall:
		go p.runInstall(seq, lang, first)
	case actionRefresh:
		go p.runRefresh(seq, lang)
	case actionTrack:
		go p.runTrack(seq, first, second, third)
	case actionUntrack:
		go p.runUntrack(seq, first)
	}
	return nil
}

func (p *Plugin) fail(_ string, err error) error {
	seq, beginErr := p.begin(plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
	if beginErr == nil {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
	}
	return err
}

func (p *Plugin) runTrack(seq uint64, name, manifestURL, publicKey string) {
	if err := p.store.Track(name, manifestURL, publicKey); err != nil {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
		return
	}
	fingerprint, _ := agentplugin.PublicKeyFingerprint(p.langOf(), publicKey)
	p.finish(seq, plugin.ActionState{Status: plugin.ActionDone,
		Message: i18n.Tf(p.langOf(), "plugin.bundles.track.done", "已为 %s 绑定更新来源，公钥指纹 %s。到对应的外源插件卡片上检查更新。", name, fingerprint)})
	p.wakeChecker()
}

func (p *Plugin) runUntrack(seq uint64, name string) {
	if err := p.store.Untrack(name); err != nil {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
		return
	}
	p.finish(seq, plugin.ActionState{Status: plugin.ActionDone,
		Message: i18n.Tf(p.langOf(), "plugin.bundles.untrack.done", "已解除 %s 的更新来源，插件包本身仍保留。", name)})
	p.wakeChecker()
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
	sb.WriteString(i18n.Tf(lang, "plugin.bundles.install.where", "\n格式：%s\n位置：%s\n", b.Format, p.store.Path(b.Manifest.Name)))
	if b.SkillsDir != "" {
		sb.WriteString(i18n.T(lang, "plugin.bundles.install.skills", "带技能：要「技能手册」插件开着才会进清单。\n"))
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
	sb.WriteString(i18n.T(lang, "plugin.bundles.install.restart_note", "\n重启服务之后它才会出现在插件列表里。"))
	p.finish(seq, plugin.ActionState{Status: plugin.ActionDone, Message: strings.TrimRight(sb.String(), "\n")})
}

func (p *Plugin) runRefresh(seq uint64, lang string) {
	list, _ := p.store.List()
	if len(list) == 0 {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionDone, Message: i18n.T(lang, "plugin.bundles.refresh.none", "一个插件包都没装。")})
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
		sb.WriteString(i18n.Tf(lang, "plugin.bundles.refresh.entry", "%s：%d 件工具\n", b.Manifest.Name, len(tools)))
		for _, m := range problems {
			fmt.Fprintf(&sb, "· × %s\n", m)
		}
	}
	if sb.Len() == 0 {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionDone, Message: i18n.T(lang, "plugin.bundles.refresh.no_mcp", "已装的插件包里没有 MCP 声明，没什么可握手的。")})
		return
	}
	sb.WriteString(i18n.Tf(lang, "plugin.bundles.refresh.total", "\n共 %d 件工具。新增的工具要重启之后才可用。", total))
	p.finish(seq, plugin.ActionState{Status: plugin.ActionDone, Message: strings.TrimRight(sb.String(), "\n")})
}

func (p *Plugin) handshake(b *agentplugin.Bundle) ([]agentplugin.ToolInfo, []string) {
	lang := p.langOf()
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	tools, problems, err := agentplugin.Handshake(ctx, lang, b, agentplugin.DataDir(p.baseDir, b.Manifest.Name))
	if err != nil {
		return nil, append(problems, err.Error())
	}
	if err := p.store.SaveTools(b.Manifest.Name, b.Manifest.Version, tools); err != nil {
		return tools, append(problems, i18n.Tf(lang, "plugin.bundles.err.save_tools", "工具清单没能存下来：%s", err.Error()))
	}
	return tools, problems
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if !knownAction(key) {
		return plugin.ActionState{}, plugin.UnknownAction(p.langOf(), key)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.actState, nil
}

func (p *Plugin) begin(st plugin.ActionState) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.actRunning {
		return 0, fmt.Errorf("%s", i18n.T(p.lang, "plugin.bundles.err.busy", "插件包管理卡上已有一项操作在进行"))
	}
	p.actSeq++
	p.actRunning = true
	p.actState = st
	return p.actSeq, nil
}

func (p *Plugin) finish(seq uint64, st plugin.ActionState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.actSeq == seq {
		p.actState = st
		p.actRunning = false
	}
}
