package selfupdate

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"wen/internal/plugin"
	"wen/internal/updater"
	"wen/internal/version"
)

// actionUpdate 是设置页上那个按钮。
//
// 检查与安装刻意共用一个入口，靠文案区分：不知道有没有新版时它是「检查更新」，
// 查到之后变成「更新到 vX 并重启」。第一次点只查不装，把版本号与更新说明摆出来，
// 第二次点才真的动手——这一步之差就是「确认」，不必再造一个确认弹窗，
// 而按钮上写着的正是即将发生的事。
const actionUpdate = "update"

func (p *Plugin) Actions() []plugin.ActionDef {
	s := p.snapshot()

	// 文案固定，当前处境放 Detail。此前这里有两套文案（查到新版时换一套，标签里还带着
	// 版本号），而语言包按键取译文，一个键只装得下一句——另一套永远没有英文，且测试
	// 看不见它（检查工具问到的是默认那一档）。运行时事实归 Detail，见 ActionDef.Detail。
	detail := "当前已是最新版。"
	if tag := s.newerTag(); tag != "" {
		detail = "查到新版 " + tag + "，点这里下载并替换。"
		if s.autoRestart && s.restart != nil {
			detail += "替换完成后服务会自动重启，正在进行的对话会被中断。"
		} else {
			detail += "替换完成后需要你自己重启程序才会生效。"
		}
	}
	return []plugin.ActionDef{{
		Key:   actionUpdate,
		Label: "检查并更新",
		Description: "查一次 GitHub 上最新的正式版。没有新版就只是查一下；" +
			"查到了会把更新说明摆出来，下载、校验与替换也在这里完成。",
		Detail: detail,
	}}
}

// StartAction 立即返回，整个流程在后台推进。
//
// 与「进行中重复触发 = 重新开始」的通用约定不同，这里的重复触发是拒绝：这个流程
// 会下载几十兆并替换程序文件，半路重来只会留下一份没人认领的下载。
func (p *Plugin) StartAction(_ context.Context, key string) error {
	if key != actionUpdate {
		return fmt.Errorf("未知的操作 %q", key)
	}
	s := p.snapshot()
	if s.exe == "" {
		return fmt.Errorf("插件尚未初始化")
	}

	install := s.newerTag() != ""
	p.actMu.Lock()
	if p.running {
		p.actMu.Unlock()
		return fmt.Errorf("更新正在进行中，请等它结束")
	}
	p.running = true
	p.actSeq++
	seq := p.actSeq
	first := "正在检查有没有新版本…"
	if install {
		first = "正在核对最新版本…"
	}
	p.actState = plugin.ActionState{Status: plugin.ActionPending, Message: first}
	p.actMu.Unlock()

	go func() {
		defer func() {
			p.actMu.Lock()
			p.running = false
			p.actMu.Unlock()
		}()
		p.run(seq, install, s)
	}()
	return nil
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if key != actionUpdate {
		return plugin.ActionState{}, fmt.Errorf("未知的操作 %q", key)
	}
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if p.actState.Status == "" {
		return plugin.ActionState{Status: plugin.ActionIdle}, nil
	}
	return p.actState, nil
}

// run 跑完一次检查（install 为 false）或一次完整更新。
func (p *Plugin) run(seq uint64, install bool, s snapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	rel, err := p.check(ctx, s)
	if err != nil {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionError, Message: "检查更新失败：" + err.Error()})
		return
	}

	if !updater.Newer(version.Version, rel.Tag) {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionDone, Message: uptodateMessage(rel.Tag)})
		return
	}
	if !install {
		// 第一次点：只报告。按钮文案随之变成「更新到 vX 并重启」，再点才装。
		// 这一条里嵌着 GitHub 的发布正文，本来就是 markdown，交给界面按 markdown 渲染。
		p.finish(seq, plugin.ActionState{
			Status: plugin.ActionDone, Message: foundMessage(rel), Markdown: true,
		})
		return
	}
	p.install(ctx, seq, s, rel)
}

// install 走完下载、校验、试运行、替换、重启这一串。
func (p *Plugin) install(ctx context.Context, seq uint64, s snapshot, rel updater.Release) {
	fail := func(err error) {
		p.finish(seq, plugin.ActionState{Status: plugin.ActionError, Message: err.Error()})
		log.Printf("self_update: 更新到 %s 失败: %v", rel.Tag, err)
	}

	// 先确认安装目录可写：装在系统目录或由包管理器装的，这一步就该停，
	// 而不是下完十几兆再来报错
	if err := updater.CheckWritable(s.exe); err != nil {
		fail(err)
		return
	}
	plan, err := s.client.Prepare(ctx, rel)
	if err != nil {
		fail(err)
		return
	}

	work := updater.WorkDir(s.exe)
	p.progress(seq, fmt.Sprintf("正在下载 %s（%s）…", plan.Asset.Name, humanSize(plan.Asset.Size)))
	archive, err := s.client.Fetch(ctx, plan, work, func(done, total int64) {
		p.progress(seq, downloadMessage(plan.Asset.Name, done, total))
	})
	if err != nil {
		fail(err)
		return
	}

	p.progress(seq, "校验通过，正在解包…")
	bin, err := updater.Extract(archive, work, updater.HostBinaryName())
	if err != nil {
		fail(err)
		return
	}

	p.progress(seq, "正在试运行新版程序…")
	if err := updater.SmokeTest(ctx, bin, rel.Tag); err != nil {
		fail(err)
		return
	}

	p.progress(seq, "正在替换程序文件…")
	if err := updater.Apply(bin, s.exe); err != nil {
		fail(err)
		return
	}

	from := version.Version
	p.mu.Lock()
	p.st.Pending = &pendingUpdate{From: from, To: rel.Tag, At: p.now()}
	p.saveLocked()
	p.mu.Unlock()
	log.Printf("self_update: 已把程序文件替换为 %s（原 %s）", rel.Tag, from)

	if !s.autoRestart || s.restart == nil {
		why := "「更新后自动重启」是关的"
		if s.restart == nil {
			why = "当前的启动方式下无法自行重启"
		}
		p.finish(seq, plugin.ActionState{
			Status:  plugin.ActionDone,
			Message: fmt.Sprintf("已更新到 %s（原 %s）。%s，重新启动程序后生效。", rel.Tag, from, why),
		})
		return
	}

	p.progress(seq, fmt.Sprintf("已更新到 %s，正在重启服务…\n重启期间界面会短暂断开，稍等几秒会自动接上。", rel.Tag))
	// 留一点时间让界面把上面这条取走：重启一发生，这个进程连同它的 HTTP 服务就没了
	select {
	case <-time.After(restartDelay):
	case <-ctx.Done():
	}
	if err := s.restart(fmt.Sprintf("更新到 %s", rel.Tag)); err != nil {
		p.finish(seq, plugin.ActionState{
			Status: plugin.ActionError,
			Message: fmt.Sprintf("已更新到 %s，但自动重启失败（%v）。手工重启程序即可用上新版。",
				rel.Tag, err),
		})
		return
	}
	// 重启成功的话进程即将消失，这里不必再写状态：新进程起来后由 reconcile
	// 按落盘的 pending 把这次操作定稿成「已完成」（见 selfupdate.go）。
}

// check 查一次最新的正式版并把结果记进状态。
//
// 无论查到的是不是新版都记：检查时刻决定下次什么时候再查，而版本号决定设置页上
// 那个按钮的文案。
func (p *Plugin) check(ctx context.Context, s snapshot) (updater.Release, error) {
	rel, err := s.client.Latest(ctx)
	if err != nil {
		return updater.Release{}, err
	}
	p.mu.Lock()
	p.st.LastCheck = p.now()
	p.st.Latest = rel.Tag
	p.st.LatestAt = rel.PublishedAt
	p.st.Notes = truncateNotes(updater.ReleaseNotes(rel.Body))
	p.saveLocked()
	p.mu.Unlock()
	return rel, nil
}

// progress 更新进展文本（仍是进行中）。
func (p *Plugin) progress(seq uint64, msg string) {
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if seq != p.actSeq {
		return
	}
	p.actState = plugin.ActionState{Status: plugin.ActionPending, Message: msg}
}

// finish 写回最终结果，只在这一次仍是最新的那次时生效。
func (p *Plugin) finish(seq uint64, st plugin.ActionState) {
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if seq != p.actSeq {
		return
	}
	p.actState = st
}

// setAction 直接落一个状态（重启后的收尾用，不属于任何一次进行中的操作）。
func (p *Plugin) setAction(st plugin.ActionState) {
	p.actMu.Lock()
	defer p.actMu.Unlock()
	p.actSeq++
	p.actState = st
}

// ---------- 文案 ----------

func uptodateMessage(latest string) string {
	cur, ok := updater.ParseVersion(version.Version)
	if ok && cur.Dev {
		return fmt.Sprintf("当前运行的是开发版 %s，它比最新的正式版 %s 还新，无需更新。",
			version.Version, latest)
	}
	return fmt.Sprintf("已是最新版 %s。", version.Version)
}

func foundMessage(rel updater.Release) string {
	var b strings.Builder
	fmt.Fprintf(&b, "发现新版 %s（当前 %s）", rel.Tag, version.Version)
	if !rel.PublishedAt.IsZero() {
		fmt.Fprintf(&b, "，发布于 %s", rel.PublishedAt.Local().Format("2006-01-02"))
	}
	b.WriteString("。\n")
	if notes := updater.ReleaseNotes(rel.Body); notes != "" {
		b.WriteString("\n")
		b.WriteString(truncateNotes(notes))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n关闭这个窗口，再点一次「更新到 %s 并重启」就开始更新。", rel.Tag)
	return b.String()
}

func downloadMessage(name string, done, total int64) string {
	if total <= 0 {
		return fmt.Sprintf("正在下载 %s：已下 %s", name, humanSize(done))
	}
	return fmt.Sprintf("正在下载 %s：%d%%（%s / %s）",
		name, done*100/total, humanSize(done), humanSize(total))
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d 字节", n)
	}
}
