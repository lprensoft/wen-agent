package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"wen/internal/agent"
	"wen/internal/clock"
	"wen/internal/config"
	"wen/internal/plugin"
	"wen/internal/stylecheck"
)

// wen eval：回放一段脚本对话，产出文风与角色一致性的报告。
//
// 调一次提示词之后角色是变好了还是变坏了，此前只能再聊几句凭感觉。这里把「再聊
// 几句」固定成脚本，跑完给出两类数字：每轮的助手腔命中与字数（纯规则，可重复），
// 以及模型对「像不像同一个人 / 语气是否一致 / 关系是否连续」的三项打分（模型打的，
// 看趋势不看绝对值）。本期只测量，不据此改任何行为。

// maxClockHours 限制脚本能把时钟推多远。五十年的小时数已经远超任何脚本的需要；
// 而再往上（约二百九十二年）换算成 time.Duration 就会溢出成负数，时钟反而往回走，
// 脚本作者看不出哪里错了。
const maxClockHours = 24 * 366 * 50

// judgeTimeout 是评判那一次模型调用的上限。
const judgeTimeout = 3 * time.Minute

// judgeMaxBytes 限制交给评判的回复总量。脚本本该是短的，这条只防有人把整个下午
// 的对话灌进来。超出时丢最早的轮次——压缩前后的对比在后半段。
const judgeMaxBytes = 120 * 1024

func runEval(args []string) error {
	// 允许脚本路径放在选项前面（wen eval x.json -o r.md）：flag 包遇到第一个
	// 非选项参数就停，所以先把它摘出来。
	var script string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		script, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	configPath := fs.String("c", "", tr("cli.flag.config", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）"))
	out := fs.String("o", "", tr("cli.eval.flag.out", "报告写入的文件（默认打到标准输出）"))
	jsonOut := fs.String("json", "", tr("cli.eval.flag.json", "把结构化结果写入文件，作为下一次对比的基线"))
	baselinePath := fs.String("baseline", "", tr("cli.eval.flag.baseline", "与上次的 --json 结果对比；确定性指标退化时退出码为 1"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	if script == "" {
		script = fs.Arg(0)
	}
	if script == "" {
		return fmt.Errorf(tr("cli.eval.usage", "用法: %s"), lookup("eval").usage)
	}

	raw, err := os.ReadFile(script)
	if err != nil {
		return fmt.Errorf(tr("cli.eval.err.read_script", "读取脚本失败: %w"), err)
	}
	sc, err := parseScript(raw)
	if err != nil {
		return fmt.Errorf(tr("cli.eval.err.script", "脚本 %s: %w"), script, err)
	}
	if sc.Name == "" {
		sc.Name = strings.TrimSuffix(filepath.Base(script), filepath.Ext(script))
	}

	cfg, err := config.Load(config.ResolvePath(*configPath))
	if err != nil {
		return fmt.Errorf(tr("cli.err.load_config", "加载配置失败: %w"), err)
	}
	adoptLang(cfg)

	// 一切落盘都进临时目录：会话、记忆库、心情、统计……评测不该在真实数据里留下
	// 任何痕迹，也不该读到真实的记忆（那会让两次评测的条件不同）。
	tmp, err := os.MkdirTemp("", "wen-eval-*")
	if err != nil {
		return fmt.Errorf(tr("cli.eval.err.tmpdir", "创建临时目录失败: %w"), err)
	}
	defer os.RemoveAll(tmp)

	rt, err := buildRuntime(cfg, runtimeOverrides{
		SessionDir: filepath.Join(tmp, "sessions"),
		PluginOpts: []plugin.Option{
			plugin.WithStateDir(filepath.Join(tmp, "plugins")),
			plugin.WithSuppressed(evalSuppressed),
		},
	})
	if err != nil {
		return err
	}
	defer rt.plugins.StopAll()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 整场评测跑在一条可以自己推进的时间线上：种子可以写在「三天前」，轮次之间的
	// gap_hours 真的把时钟拨过去。真实路径没有这条线，插件读到的仍是系统时间。
	clk := clock.NewManual(sc.startAt())
	ctx = clock.With(ctx, clk)

	if len(sc.Seed) > 0 {
		if err := runSeeds(ctx, rt, clk, sc.Seed); err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, trf("cli.eval.seed.done", "预置状态：%d 条\n", len(sc.Seed)))
	}

	// 基线先读、先校验，再开跑：脚本名对不上时当场报错，别烧完一整轮模型调用才发现。
	var base evalJSON
	if *baselinePath != "" {
		base, err = loadEvalJSON(*baselinePath)
		if err != nil {
			return err
		}
		if base.Name != "" && sc.Name != "" && base.Name != sc.Name {
			return fmt.Errorf(tr("cli.eval.err.baseline_name", "基线来自另一个脚本（%s），不能与 %s 对比"), base.Name, sc.Name)
		}
		if err := checkBaselineTurns(sc, base); err != nil {
			return err
		}
		if cur := startOf(sc.startAt()); base.Start != "" && base.Start != cur {
			fmt.Fprint(os.Stderr, trf("cli.eval.baseline_clock", "注意：基线从 %s 开始，本次从 %s 开始；场景不同，命中与评分未必可比\n", base.Start, cur))
		}
		if model := rt.current.ProviderName + "/" + rt.current.ModelID; base.Model != "" && base.Model != model {
			fmt.Fprint(os.Stderr, trf("cli.eval.baseline_model", "注意：基线用的是 %s，本次是 %s；评分与命中都可能不可比\n", base.Model, model))
		}
	}

	res := runScript(ctx, rt, sc, clk, os.Stderr)
	if *baselinePath != "" {
		res.BaselinePath = *baselinePath
		res.Regressions, res.JudgeDelta = compareEvalBaseline(evalJSONOf(res), base)
	}
	if *jsonOut != "" {
		if err := writeEvalJSON(*jsonOut, res); err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, trf("cli.eval.json_written", "结构化结果已写入 %s\n", *jsonOut))
	}
	report := renderEvalReport(res)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
			return fmt.Errorf(tr("cli.eval.err.write_report", "写报告失败: %w"), err)
		}
		fmt.Fprint(os.Stderr, trf("cli.eval.report_written", "报告已写入 %s\n", *out))
	} else {
		fmt.Print(report)
	}
	if res.failed() {
		if res.turnFailed() {
			return fmt.Errorf("%s", tr("cli.eval.err.had_failure", "有轮次失败，见报告"))
		}
		return fmt.Errorf("%s", tr("cli.eval.err.gate_failed", "门禁未通过，见报告"))
	}
	return nil
}

// evalSuppressed 圈出评测时不启动的插件：消息通道会连到真实平台，后台任务会在评测
// 会话上插话（心跳挑的正是「最近活跃的会话」，评测会话在临时目录里就是唯一的那个），
// 程序维护会在评测中途去够外网、甚至把脚下的二进制换掉。
// 按分组判断而不是点名：以后再加一条 IM 或一种后台任务，这里不用改。
func evalSuppressed(p plugin.Plugin) bool {
	switch plugin.CategoryOf(p) {
	case plugin.CategoryBackground, plugin.CategoryChannel, plugin.CategoryProgram:
		return true
	}
	return false
}

// runSeeds 在开跑之前把预置状态写进评测环境。
//
// 走插件自己的工具而不是直接写状态文件：不复制十几个插件的落盘格式，种子也走真实
// 的写入路径；以后加插件不用改这里。带 ago_hours 的种子把时钟拨回那一刻再写入，
// 于是「三天前记住的」在库里就是三天前的：淡忘、索引排序、快照里的「N 天前」都对得上。
func runSeeds(ctx context.Context, rt *runtime, clk *clock.Manual, seeds []evalSeed) error {
	for i, sd := range seeds {
		tool, ok := rt.plugins.FindTool(sd.Tool)
		if !ok {
			return fmt.Errorf(tr("cli.eval.err.seed_tool", "seed[%d]: 没有工具 %q"), i+1, sd.Tool)
		}
		args := []byte("{}")
		if len(sd.Args) > 0 {
			raw, err := json.Marshal(sd.Args)
			if err != nil {
				return fmt.Errorf(tr("cli.eval.err.seed_args", "seed[%d]（%s）的参数不是合法 JSON: %w"), i+1, sd.Tool, err)
			}
			args = raw
		}
		c := ctx
		if sd.Scope != "" {
			c = plugin.WithScope(c, plugin.Scope{Write: sd.Scope, Read: []string{sd.Scope}})
		}
		at := clk.Now()
		if sd.AgoHours > 0 {
			clk.Set(at.Add(-time.Duration(sd.AgoHours * float64(time.Hour))))
		}
		_, err := tool.Execute(c, args)
		clk.Set(at) // 无论成败都拨回来：后面的种子与轮次都从脚本的起点继续
		if err != nil {
			return fmt.Errorf(tr("cli.eval.err.seed_failed", "seed[%d]（%s）失败: %w"), i+1, sd.Tool, err)
		}
	}
	return nil
}

// runScript 跑完整个脚本并收集结果。progress 收每轮的进度提示，可为 nil。
func runScript(ctx context.Context, rt *runtime, sc evalScript, clk *clock.Manual, progress *os.File) evalResult {
	res := evalResult{
		Name:       sc.Name,
		Model:      rt.current.ProviderName + "/" + rt.current.ModelID,
		StartedAt:  time.Now(),
		ClockFrom:  clk.Now(),
		ClockFixed: !sc.start.IsZero(),
	}
	res.Persona, res.Samples, res.HasRoleplay = roleplayTexts(rt.plugins)

	say := func(format string, a ...any) {
		if progress != nil {
			fmt.Fprintf(progress, format+"\n", a...)
		}
	}

	meta, err := rt.store.Create()
	if err != nil {
		res.SetupErr = trf("cli.eval.err.new_session", "新建会话失败: %s", err.Error())
		return res
	}
	sessionID := meta.ID

	turnNo := 0
	for _, step := range sc.Turns {
		if ctx.Err() != nil {
			res.Rows = append(res.Rows, evalRow{Kind: rowSay, Index: turnNo + 1, Err: tr("cli.eval.aborted", "已中断")})
			break
		}
		switch {
		case step.Say != "":
			turnNo++
			say(tr("cli.eval.say.turn", "第 %d 轮：%s"), turnNo, clipLine(step.Say, 30))
			row := runSayTurn(plugin.WithInteractive(ctx), rt.agent, sessionID, turnNo, step.Say, step.Expect)
			res.Rows = append(res.Rows, row.rows...)
			res.AutoCompactions += row.autoCompactions
			if row.failed {
				say(tr("cli.eval.say.failed", "  失败：%s"), row.rows[0].Err)
				return res // 后面的轮次建立在这一轮之上，失败了再跑没有意义
			}
		case step.Compact:
			say("%s", tr("cli.eval.say.compacting", "压缩…"))
			row := evalRow{Kind: rowCompact}
			rt.agent.Compact(ctx, sessionID, func(ev agent.Event) {
				if ev.Type == agent.EventError {
					row.Err = ev.Error
				}
			})
			if row.Err == "" {
				res.Compactions++
			}
			res.Rows = append(res.Rows, row)
		case step.GapHours > 0:
			clk.Advance(time.Duration(step.GapHours * float64(time.Hour)))
			res.Rows = append(res.Rows, evalRow{Kind: rowGap, GapHours: step.GapHours})
		}
	}
	res.Duration = time.Since(res.StartedAt)
	res.ClockTo = clk.Now()

	if ctx.Err() != nil {
		res.Judge.Err = tr("cli.eval.aborted_judge", "已中断，未评判")
		return res
	}
	say("%s", tr("cli.eval.say.judging", "评判中…"))
	jctx, cancel := context.WithTimeout(ctx, judgeTimeout)
	defer cancel()
	prompt := buildJudgePrompt(res.Persona, res.Samples, res.Rows, sc.JudgePoints)
	raw, err := rt.agent.Complete(jctx, prompt)
	if err != nil {
		res.Judge.Err = err.Error()
		return res
	}
	j, err := parseJudge(raw)
	if err != nil {
		res.Judge.Err = err.Error()
		res.Judge.Raw = raw
		return res
	}
	res.Judge = j
	return res
}

// sayOutcome 是一轮 say 的产出：一行回复记录，后面可能跟着一行自动压缩的记录。
type sayOutcome struct {
	rows            []evalRow
	autoCompactions int
	failed          bool
}

// runSayTurn 跑一轮对话。最终文本从事件流里取：每次工具调用开始就清一次缓冲，
// 留下的就是最后那段回复。
func runSayTurn(ctx context.Context, ag *agent.Agent, sessionID string, index int, input string, exp *evalExpect) sayOutcome {
	var (
		buf     strings.Builder
		errText string
		compact bool
		tools   []string
	)
	ag.Run(ctx, sessionID, input, func(ev agent.Event) {
		switch ev.Type {
		case agent.EventDelta:
			buf.WriteString(ev.Content)
		case agent.EventToolStart:
			tools = append(tools, ev.ToolName)
			buf.Reset()
		case agent.EventCompactStart:
			compact = true
		case agent.EventError:
			errText = ev.Error
		}
	})
	row := evalRow{Kind: rowSay, Index: index, Say: input, Reply: strings.TrimSpace(buf.String()), Tools: tools, Err: errText}
	if errText != "" {
		return sayOutcome{rows: []evalRow{row}, failed: true}
	}
	row.Metrics = stylecheck.Measure(uiLang, row.Reply)
	row.Hits = stylecheck.Check(uiLang, row.Reply)
	if exp != nil {
		row.ExpectFails = evalExpectFails(row, exp)
		if row.ExpectFails == nil {
			// 写了 expect 但全部通过：留一个空切片，报告才能区分「没断言」与「断言全过」。
			row.ExpectFails = []string{}
		}
	}
	out := sayOutcome{rows: []evalRow{row}}
	if compact {
		out.rows = append(out.rows, evalRow{Kind: rowAutoCompact})
		out.autoCompactions = 1
	}
	return out
}

// roleplayTexts 从 roleplay 插件的生效配置里取角色设定与台词样例，交给评判。
// 走 List 而不是直接碰插件：这里只认「叫 roleplay 的那个插件的配置」，不依赖它的类型。
func roleplayTexts(m *plugin.Manager) (persona, samples string, enabled bool) {
	for _, st := range m.List() {
		if st.Name != "roleplay" {
			continue
		}
		return plugin.CfgString(st.Config, "persona", ""), plugin.CfgString(st.Config, "voice_samples", ""), st.Enabled
	}
	return "", "", false
}

// clipLine 把一段话压成一行短文，给进度提示用。
func clipLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// ---------- 脚本 ----------

// evalScript 是脚本文件的形状。
type evalScript struct {
	Name string `json:"name"`
	// Start 是虚拟时钟的起点（RFC3339）。不写就从当下开始；写了才能让报告里的
	// 日期与「今天是几号」这类判断每次运行都一致。
	Start       string     `json:"start,omitempty"`
	Seed        []evalSeed `json:"seed,omitempty"`
	Turns       []evalStep `json:"turns"`
	JudgePoints []string   `json:"judge_points,omitempty"`

	start time.Time // Start 解析后的值，零值表示脚本没写
}

// startAt 返回虚拟时钟的起点。
func (s evalScript) startAt() time.Time {
	if s.start.IsZero() {
		return time.Now()
	}
	return s.start
}

// evalSeed 是一条预置状态：用插件的某个工具往评测环境里写一条。
//
// 走工具而不是直接写状态文件：不复制每个插件的落盘格式，种子也走真实写入路径。
// 时间相关的种子（「三天前记住的」）要等虚拟时钟。
type evalSeed struct {
	Tool  string         `json:"tool"`
	Args  map[string]any `json:"args,omitempty"`
	Scope string         `json:"scope,omitempty"` // 可见域；空 = 共享
	// AgoHours 让这条状态写在现在之前多少小时：72 就是「三天前记住的」。
	// 淡忘、索引排序、快照里的「N 天前」都按这个时刻算。
	AgoHours float64 `json:"ago_hours,omitempty"`
}

// evalExpect 是一轮回复的确定性断言。全部为空表示不断言。
type evalExpect struct {
	MustInclude    []string       `json:"must_include,omitempty"`
	MustNotInclude []string       `json:"must_not_include,omitempty"`
	MaxHits        map[string]int `json:"max_hits,omitempty"`
	MaxSentences   int            `json:"max_sentences,omitempty"`
	MustCall       []string       `json:"must_call,omitempty"`
}

// evalStep 是脚本里的一步：三者取其一。
type evalStep struct {
	Say      string      `json:"say,omitempty"`
	Compact  bool        `json:"compact,omitempty"`
	GapHours float64     `json:"gap_hours,omitempty"`
	Expect   *evalExpect `json:"expect,omitempty"`
}

// parseScript 解析并校验脚本。
func parseScript(raw []byte) (evalScript, error) {
	var sc evalScript
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return sc, fmt.Errorf(tr("cli.eval.err.bad_json", "不是合法的脚本 JSON: %w"), err)
	}
	if len(sc.Turns) == 0 {
		return sc, fmt.Errorf("%s", tr("cli.eval.err.no_turns", "turns 为空"))
	}
	if s := strings.TrimSpace(sc.Start); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return sc, fmt.Errorf(tr("cli.eval.err.bad_start", "start 不是合法的 RFC3339 时间（如 2026-03-01T08:00:00+08:00）: %w"), err)
		}
		sc.start = t
	}
	for i, sd := range sc.Seed {
		if strings.TrimSpace(sd.Tool) == "" {
			return sc, fmt.Errorf(tr("cli.eval.err.seed_tool_empty", "seed[%d]: tool 不能为空"), i+1)
		}
		sc.Seed[i].Tool = strings.TrimSpace(sd.Tool)
		// 种子直接构造可见域，绕过了 Manager.DecideScope 的校验；而标签会被拼进
		// DomainDir 的磁盘路径，所以这里必须自己拦住 "x/../../outside" 这类值。
		sc.Seed[i].Scope = strings.TrimSpace(sd.Scope)
		if !plugin.ValidDomainTag(sc.Seed[i].Scope) {
			return sc, fmt.Errorf(tr("cli.eval.err.seed_scope", "seed[%d]: 可见域 %q 不合法，只允许小写字母开头的 [a-z0-9_]"), i+1, sd.Scope)
		}
		if sd.AgoHours < 0 {
			return sc, fmt.Errorf(tr("cli.eval.err.seed_ago_neg", "seed[%d]: ago_hours 不能为负"), i+1)
		}
		if sd.AgoHours > maxClockHours {
			return sc, fmt.Errorf(tr("cli.eval.err.seed_ago_long", "seed[%d]: ago_hours 超过 %d 小时"), i+1, maxClockHours)
		}
	}
	says := 0
	for i, st := range sc.Turns {
		n := 0
		if strings.TrimSpace(st.Say) != "" {
			n++
			says++
		}
		if st.Compact {
			n++
		}
		if st.GapHours != 0 {
			if st.GapHours < 0 {
				return sc, fmt.Errorf(tr("cli.eval.err.neg_gap", "turns[%d]: gap_hours 不能为负"), i)
			}
			if st.GapHours > maxClockHours {
				return sc, fmt.Errorf(tr("cli.eval.err.gap_long", "turns[%d]: gap_hours 超过 %d 小时"), i, maxClockHours)
			}
			n++
		}
		if n != 1 {
			return sc, fmt.Errorf(tr("cli.eval.err.bad_step", "turns[%d]: 每步只能是 say、compact、gap_hours 之一"), i)
		}
		if st.Expect != nil {
			if strings.TrimSpace(st.Say) == "" {
				return sc, fmt.Errorf(tr("cli.eval.err.expect_not_say", "turns[%d]: expect 只能用在 say 上"), i)
			}
			if err := validateExpect(i, st.Expect); err != nil {
				return sc, err
			}
		}
		sc.Turns[i].Say = strings.TrimSpace(st.Say)
	}
	if says == 0 {
		return sc, fmt.Errorf("%s", tr("cli.eval.err.no_say", "脚本里没有任何一句 say"))
	}
	return sc, nil
}

// validateExpect 在开跑前拦住写错的断言：规则 id 拼错、上限写成负数，都该在脚本
// 解析时就说清楚，而不是跑完一次模型调用才发现什么都没断言上。
func validateExpect(i int, e *evalExpect) error {
	for _, list := range [][]string{e.MustInclude, e.MustNotInclude, e.MustCall} {
		for _, s := range list {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf(tr("cli.eval.err.expect_empty", "turns[%d]: 断言里有空字符串"), i)
			}
		}
	}
	for id, n := range e.MaxHits {
		if !stylecheck.Known(id) {
			return fmt.Errorf(tr("cli.eval.err.expect_rule", "turns[%d]: 未知的命中规则 %q"), i, id)
		}
		if n < 0 {
			return fmt.Errorf(tr("cli.eval.err.expect_hits_neg", "turns[%d]: max_hits[%q] 不能为负"), i, id)
		}
	}
	if e.MaxSentences < 0 {
		return fmt.Errorf(tr("cli.eval.err.expect_sentences_neg", "turns[%d]: max_sentences 不能为负"), i)
	}
	return nil
}
