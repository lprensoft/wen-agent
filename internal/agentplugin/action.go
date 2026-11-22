package agentplugin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wen/internal/plugin"

	"wen/internal/i18n"
)

const (
	actionRefresh      = "refresh"
	actionCheckUpdate  = "check"
	actionStageUpdate  = "update"
	actionCancelUpdate = "cancel_update"
)

const (
	handshakeTimeout   = 90 * time.Second
	updateCheckTimeout = 30 * time.Second
	updateStageTimeout = 5 * time.Minute
)

// actionState 按操作分别保存进展，同时用 running 拒绝同一包上的并发写操作。
type actionState struct {
	mu      sync.Mutex
	seq     uint64
	running bool
	states  map[string]plugin.ActionState
}

func knownAction(key string) bool {
	switch key {
	case actionRefresh, actionCheckUpdate, actionStageUpdate, actionCancelUpdate:
		return true
	}
	return false
}

func (p *Plugin) Actions() []plugin.ActionDef {
	var out []plugin.ActionDef
	if p.store != nil {
		src, _ := p.store.Source(p.bundle.Manifest.Name)
		pending, _ := p.store.Pending(p.bundle.Manifest.Name)
		switch {
		case pending != nil:
			out = append(out, plugin.ActionDef{
				Key:         actionCancelUpdate,
				Label:       i18n.T(p.langOf(), "plugin.external.action.cancel_update.label", "取消待更新"),
				Description: i18n.T(p.langOf(), "plugin.external.action.cancel_update.desc", "删除尚未应用的暂存更新，继续保留当前版本。"),
				Detail:      pending.FromVersion + " → " + pending.Version,
				Token:       pending.actionToken(),
			})
		case src != nil:
			out = append(out, plugin.ActionDef{
				Key:         actionCheckUpdate,
				Label:       i18n.T(p.langOf(), "plugin.external.action.check.label", "检查更新"),
				Description: i18n.T(p.langOf(), "plugin.external.action.check.desc", "下载更新清单与签名，用绑定的发布者公钥验签；只检查，不下载插件包。"),
				Detail:      src.ManifestURL,
			})
			if src.Candidate != nil {
				detail := p.bundle.Manifest.Version + " → " + src.Candidate.Version
				if fingerprint, err := PublicKeyFingerprint(p.langOf(), src.PublicKey); err == nil {
					detail += " · " + fingerprint
				}
				out = append(out, plugin.ActionDef{
					Key:         actionStageUpdate,
					Label:       i18n.T(p.langOf(), "plugin.external.action.update.label", "更新"),
					Description: i18n.T(p.langOf(), "plugin.external.action.update.desc", "下载并暂存这个已验签的版本，下一次重启服务时替换。不会自动执行新包里的 MCP 命令。"),
					Detail:      detail, Token: src.Candidate.Token,
				})
			}
		}
	}
	if len(p.bundle.Servers) > 0 {
		detail := fmt.Sprintf("· %d server · %d tools", len(p.bundle.Servers), len(p.tools))
		out = append(out, plugin.ActionDef{
			Key:         actionRefresh,
			Label:       i18n.T(p.langOf(), "plugin.bundle.action.label", "重新握手"),
			Description: i18n.T(p.langOf(), "plugin.bundle.action.desc", "连一遍这个包里的 MCP server，把它们提供的工具重新记下来。"),
			Detail:      detail,
		})
	}
	return out
}

// StartAction 立即返回，网络与文件暂存都在后台进行。
func (p *Plugin) StartAction(ctx context.Context, key string) error {
	if !knownAction(key) {
		return plugin.UnknownAction(p.langOf(), key)
	}
	lang := p.langOf()
	if key != actionRefresh && p.store == nil {
		return fmt.Errorf("%s", i18n.T(lang, "plugin.external.action.no_store", "这个插件包没有可用的更新存放目录"))
	}
	var dataDir string
	if key == actionRefresh {
		p.mu.RLock()
		dataDir = p.stateDir
		p.mu.RUnlock()
		if dataDir == "" {
			return fmt.Errorf("%s", i18n.T(lang, "plugin.bundle.action.not_ready", "这个插件还没初始化，先启用它再握手"))
		}
		dataDir = filepath.Join(dataDir, DataDirName)
	}
	token := plugin.ActionTokenFrom(ctx)
	// 取消按钮绑定的是展示时那一份 pending。旧窗口没有凭据，不能让它按名字取消
	// 「当前」那一份——那可能已经是另一个窗口换过的版本。
	if key == actionCancelUpdate && token == "" {
		return fmt.Errorf("%s", i18n.T(lang, "plugin.external.action.cancel_no_token", "这个页面没有取消凭据，刷新后再试。"))
	}

	pendingMessage := i18n.T(lang, "plugin.external.action.started", "操作已开始 …")
	if key == actionRefresh {
		pendingMessage = i18n.Tf(lang, "plugin.bundle.action.connecting", "正在连接 %d 台 server …", len(p.bundle.Servers))
	}
	seq, err := p.beginAction(key, plugin.ActionState{Status: plugin.ActionPending, Message: pendingMessage})
	if err != nil {
		return err
	}
	switch key {
	case actionRefresh:
		go p.runRefresh(key, seq, lang, dataDir)
	case actionCheckUpdate:
		go p.runCheck(key, seq, lang)
	case actionStageUpdate:
		go p.runStage(key, seq, lang, token)
	case actionCancelUpdate:
		go p.runCancel(key, seq, lang, token)
	}
	return nil
}

func (p *Plugin) runCancel(key string, seq uint64, lang, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), updateStageTimeout)
	defer cancel()
	err := p.store.CancelUpdateContext(ctx, p.bundle.Manifest.Name, token)
	st := plugin.ActionState{Status: plugin.ActionDone,
		Message: i18n.T(lang, "plugin.external.action.cancelled", "待更新已取消，当前包保持不变。")}
	if err != nil {
		st = plugin.ActionState{Status: plugin.ActionError, Message: err.Error()}
	}
	p.finishAction(key, seq, st)
}

func (p *Plugin) runCheck(key string, seq uint64, lang string) {
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()
	candidate, err := p.store.CheckUpdate(ctx, p.bundle.Manifest.Name)
	if err != nil {
		p.finishAction(key, seq, plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
		return
	}
	message := i18n.T(lang, "plugin.external.action.latest", "签名验证通过，当前已经是最新版。")
	if candidate != nil {
		message = i18n.Tf(lang, "plugin.external.action.available", "签名验证通过，可以更新到 %s。刷新声明后点「更新」继续。", candidate.Version)
	}
	p.finishAction(key, seq, plugin.ActionState{Status: plugin.ActionDone, Message: message})
}

func (p *Plugin) runStage(key string, seq uint64, lang, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), updateStageTimeout)
	defer cancel()
	pending, err := p.store.StageUpdate(ctx, p.bundle.Manifest.Name, token)
	if err != nil {
		p.finishAction(key, seq, plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
		return
	}
	message := i18n.Tf(lang, "plugin.external.action.staged", "%s 已完整暂存，当前包尚未改动；下一次重启服务时应用。", pending.Version)
	if p.bundle.HasMCP {
		message += i18n.T(lang, "plugin.external.action.staged_mcp", "\n新版应用后请人工重新握手；新工具清单再经一次重启后注册。更新过程没有执行新包里的命令。")
	}
	p.finishAction(key, seq, plugin.ActionState{Status: plugin.ActionDone, Message: message})
}

func (p *Plugin) runRefresh(key string, seq uint64, lang, dataDir string) {
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	tools, problems, err := Handshake(ctx, lang, p.bundle, dataDir)
	if err != nil {
		p.finishAction(key, seq, plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
		return
	}
	if p.store != nil {
		if err := p.store.SaveTools(p.bundle.Manifest.Name, p.bundle.Manifest.Version, tools); err != nil {
			p.finishAction(key, seq, plugin.ActionState{Status: plugin.ActionError, Message: i18n.Tf(lang, "plugin.bundle.action.save_failed", "工具清单没能存下来：%s", err.Error())})
			return
		}
	}

	var b strings.Builder
	if len(tools) == 0 {
		b.WriteString(i18n.T(lang, "plugin.bundle.action.no_tools", "一件工具都没取到。\n"))
	} else {
		b.WriteString(i18n.Tf(lang, "plugin.bundle.action.got", "取到 %d 件工具：\n", len(tools)))
		for _, t := range tools {
			fmt.Fprintf(&b, "· %s\n", t.Local)
		}
	}
	if len(problems) > 0 {
		b.WriteString(i18n.Tf(lang, "plugin.bundle.action.failed_n", "\n有 %d 台没能连上：\n", len(problems)))
		for _, m := range problems {
			fmt.Fprintf(&b, "· %s\n", m)
		}
	}
	b.WriteString(i18n.T(lang, "plugin.bundle.action.saved", "\n清单已存下。工具表在服务启动时定下，重启之后这些工具才会真的可用。"))

	st := plugin.ActionState{Status: plugin.ActionDone, Message: strings.TrimRight(b.String(), "\n")}
	if len(tools) == 0 && len(problems) > 0 {
		st.Status = plugin.ActionError
	}
	p.finishAction(key, seq, st)
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if !knownAction(key) {
		return plugin.ActionState{}, plugin.UnknownAction(p.langOf(), key)
	}
	p.act.mu.Lock()
	defer p.act.mu.Unlock()
	return p.act.states[key], nil
}

func (p *Plugin) beginAction(key string, st plugin.ActionState) (uint64, error) {
	p.act.mu.Lock()
	defer p.act.mu.Unlock()
	if p.act.running {
		return 0, fmt.Errorf("%s", i18n.T(p.langOf(), "plugin.external.action.busy", "这个插件包已有一项操作在进行，等它结束后再试"))
	}
	p.act.seq++
	p.act.running = true
	if p.act.states == nil {
		p.act.states = make(map[string]plugin.ActionState)
	}
	p.act.states[key] = st
	return p.act.seq, nil
}

func (p *Plugin) finishAction(key string, seq uint64, st plugin.ActionState) {
	p.act.mu.Lock()
	defer p.act.mu.Unlock()
	if p.act.seq == seq {
		p.act.states[key] = st
		p.act.running = false
	}
}
