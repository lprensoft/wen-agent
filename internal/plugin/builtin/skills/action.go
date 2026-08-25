package skills

import (
	"context"
	"fmt"
	"strings"

	"wen/internal/plugin"
	"wen/internal/skillroots"
)

// actionRescan 是设置页上的「扫描技能目录」按钮。
//
// 它同时担着两件事：把新放进去的技能加载进来（不必重启），以及告诉用户技能目录到底
// 在哪儿、每个文件被解析成了什么。后一件是这个插件的主要发现路径——技能是用户自己
// 往目录里放的文件，而那个目录在配置目录深处，不说清楚没人找得到。
const actionRescan = "rescan"

func (p *Plugin) Actions() []plugin.ActionDef {
	p.mu.RLock()
	dir := p.dir
	p.mu.RUnlock()

	desc := "每个技能是技能目录下的一个子目录，里面放一个 SKILL.md，开头用 --- 包起来的说明块里写 description 说明它是干什么用的。" +
		"放好文件后点这里即可加载，不必重启。"
	if dir != "" {
		desc = "当前技能目录：" + dir + "\n" + desc
	}
	return []plugin.ActionDef{{
		Key:         actionRescan,
		Label:       "扫描技能目录",
		Description: desc,
	}}
}

// StartAction 立即返回，扫描在后台进行。
func (p *Plugin) StartAction(ctx context.Context, key string) error {
	if key != actionRescan {
		return fmt.Errorf("未知的操作 %q", key)
	}
	p.mu.RLock()
	saved, maxList, maxDesc := p.dir, p.maxList, maxDescRunes
	p.mu.RUnlock()

	// 草稿值必须在这里同步取出：ctx 属于那个 HTTP 请求，响应发出后就失效了，
	// 而扫描在后台 goroutine 里跑。改了目录还没保存时，先扫给用户看能不能用。
	draft := strings.TrimSpace(plugin.ActionValueOr(plugin.ActionValuesFrom(ctx), "dir", ""))
	target, live := saved, true
	if draft != "" && draft != saved {
		target, live = draft, false
	}
	if target == "" {
		err := fmt.Errorf("技能目录尚未确定，请先保存一次配置")
		p.setActionState(plugin.ActionState{Status: plugin.ActionError, Message: err.Error() + "。"})
		return err
	}

	seq := p.beginAction(plugin.ActionState{
		Status:  plugin.ActionPending,
		Message: "正在扫描 " + target + " …",
	})
	go p.runRescan(seq, target, live, maxList, maxDesc)
	return nil
}

// runRescan 扫一遍目录并报告结果。live 为真时把结果写回生效状态——
// 扫的是尚未保存的草稿目录时不写：那还不是用户的决定，先看结果再决定要不要保存。
func (p *Plugin) runRescan(seq uint64, dir string, live bool, maxList, maxDesc int) {
	res := scan(dir, maxDesc)
	if live {
		// 走完整的 rescan 而不是把这一趟的结果直接写回：生效清单是「用户自己的
		// 目录 + 各插件包登记的目录」合起来的，只写这一趟等于把插件包里的技能
		// 全抹掉，而按钮上写的是「扫描技能目录」。
		p.rescan()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "技能目录：%s\n\n", dir)
	// 每一段都以换行结尾，后面的段落才能靠开头的空行与它隔开
	switch {
	case res.missing:
		b.WriteString("这个目录不存在。请先建好它，再把技能放进去。\n")
	case len(res.skills) == 0:
		b.WriteString("没有找到可用的技能。\n")
	default:
		fmt.Fprintf(&b, "加载了 %d 个技能：\n", len(res.skills))
		for _, s := range res.skills {
			fmt.Fprintf(&b, "· %s：%s\n", s.Name, s.Desc)
		}
		if maxList > 0 && len(res.skills) > maxList {
			fmt.Fprintf(&b, "\n常驻清单只列前 %d 个，其余的模型可用 list_skills 查到。\n", maxList)
		}
	}
	// 问题单独列出：一处坏掉不影响其余，但得让人知道坏在哪儿
	if len(res.problems) > 0 && !res.missing {
		fmt.Fprintf(&b, "\n有 %d 处没能加载：\n", len(res.problems))
		for _, m := range res.problems {
			fmt.Fprintf(&b, "· %s\n", m)
		}
	}
	if !live {
		b.WriteString("\n以上是对尚未保存的目录的试扫，当前生效的技能没有变动。保存配置后才会切过去。")
	}
	// 插件包带进来的技能不在这一趟的结果里（这里扫的是技能目录本身），
	// 但它们确实在生效清单中——不说一句的话，界面上的条数会对不上。
	if n := len(skillroots.List()); n > 0 {
		fmt.Fprintf(&b, `
另有 %d 个插件包各自带着技能，它们与上面这些合成同一份清单。`, n)
	}

	st := plugin.ActionState{Status: plugin.ActionDone, Message: strings.TrimRight(b.String(), "\n")}
	if res.missing || (len(res.skills) == 0 && len(res.problems) > 0) {
		st.Status = plugin.ActionError
	}
	p.finishAction(seq, st)
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if key != actionRescan {
		return plugin.ActionState{}, fmt.Errorf("未知的操作 %q", key)
	}
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if p.actState.Status == "" {
		return plugin.ActionState{Status: plugin.ActionIdle}, nil
	}
	return p.actState, nil
}

// beginAction 记下新一次扫描的状态并返回它的序号：进行中重复触发 = 重新开始，
// 先发的那次若后回来，不能拿旧结果盖掉新的。
func (p *Plugin) beginAction(st plugin.ActionState) uint64 {
	p.actMu.Lock()
	defer p.actMu.Unlock()
	p.actSeq++
	p.actState = st
	return p.actSeq
}

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
