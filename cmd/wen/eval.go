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
	configPath := fs.String("c", "", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）")
	out := fs.String("o", "", "报告写入的文件（默认打到标准输出）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if script == "" {
		script = fs.Arg(0)
	}
	if script == "" {
		return fmt.Errorf("用法: %s", lookup("eval").usage)
	}

	raw, err := os.ReadFile(script)
	if err != nil {
		return fmt.Errorf("读取脚本失败: %w", err)
	}
	sc, err := parseScript(raw)
	if err != nil {
		return fmt.Errorf("脚本 %s: %w", script, err)
	}
	if sc.Name == "" {
		sc.Name = strings.TrimSuffix(filepath.Base(script), filepath.Ext(script))
	}

	cfg, err := config.Load(config.ResolvePath(*configPath))
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	adoptLang(cfg)

	// 一切落盘都进临时目录：会话、记忆库、心情、统计……评测不该在真实数据里留下
	// 任何痕迹，也不该读到真实的记忆（那会让两次评测的条件不同）。
	tmp, err := os.MkdirTemp("", "wen-eval-*")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
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

	res := runScript(ctx, rt, sc, os.Stderr)
	report := renderEvalReport(res)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
			return fmt.Errorf("写报告失败: %w", err)
		}
		fmt.Fprintf(os.Stderr, "报告已写入 %s\n", *out)
	} else {
		fmt.Print(report)
	}
	if res.failed() {
		return fmt.Errorf("有轮次失败，见报告")
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

// runScript 跑完整个脚本并收集结果。progress 收每轮的进度提示，可为 nil。
func runScript(ctx context.Context, rt *runtime, sc evalScript, progress *os.File) evalResult {
	res := evalResult{
		Name:      sc.Name,
		Model:     rt.current.ProviderName + "/" + rt.current.ModelID,
		StartedAt: time.Now(),
	}
	res.Persona, res.Samples, res.HasRoleplay = roleplayTexts(rt.plugins)

	say := func(format string, a ...any) {
		if progress != nil {
			fmt.Fprintf(progress, format+"\n", a...)
		}
	}

	meta, err := rt.store.Create()
	if err != nil {
		res.SetupErr = "新建会话失败: " + err.Error()
		return res
	}
	sessionID := meta.ID

	turnNo := 0
	for _, step := range sc.Turns {
		if ctx.Err() != nil {
			res.Rows = append(res.Rows, evalRow{Kind: rowSay, Index: turnNo + 1, Err: "已中断"})
			break
		}
		switch {
		case step.Say != "":
			turnNo++
			say("第 %d 轮：%s", turnNo, clipLine(step.Say, 30))
			row := runSayTurn(plugin.WithInteractive(ctx), rt.agent, sessionID, turnNo, step.Say)
			res.Rows = append(res.Rows, row.rows...)
			res.AutoCompactions += row.autoCompactions
			if row.failed {
				say("  失败：%s", row.rows[0].Err)
				return res // 后面的轮次建立在这一轮之上，失败了再跑没有意义
			}
		case step.Compact:
			say("压缩…")
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
			res.Rows = append(res.Rows, evalRow{Kind: rowGap, GapHours: step.GapHours})
		}
	}
	res.Duration = time.Since(res.StartedAt)

	if ctx.Err() != nil {
		res.Judge.Err = "已中断，未评判"
		return res
	}
	say("评判中…")
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
func runSayTurn(ctx context.Context, ag *agent.Agent, sessionID string, index int, input string) sayOutcome {
	var (
		buf     strings.Builder
		errText string
		compact bool
	)
	ag.Run(ctx, sessionID, input, func(ev agent.Event) {
		switch ev.Type {
		case agent.EventDelta:
			buf.WriteString(ev.Content)
		case agent.EventToolStart:
			buf.Reset()
		case agent.EventCompactStart:
			compact = true
		case agent.EventError:
			errText = ev.Error
		}
	})
	row := evalRow{Kind: rowSay, Index: index, Say: input, Reply: strings.TrimSpace(buf.String()), Err: errText}
	if errText != "" {
		return sayOutcome{rows: []evalRow{row}, failed: true}
	}
	row.Metrics = stylecheck.Measure(row.Reply)
	row.Hits = stylecheck.Check(row.Reply)
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
	Name        string     `json:"name"`
	Turns       []evalStep `json:"turns"`
	JudgePoints []string   `json:"judge_points,omitempty"`
}

// evalStep 是脚本里的一步：三者取其一。
type evalStep struct {
	Say      string  `json:"say,omitempty"`
	Compact  bool    `json:"compact,omitempty"`
	GapHours float64 `json:"gap_hours,omitempty"`
}

// parseScript 解析并校验脚本。
func parseScript(raw []byte) (evalScript, error) {
	var sc evalScript
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return sc, fmt.Errorf("不是合法的脚本 JSON: %w", err)
	}
	if len(sc.Turns) == 0 {
		return sc, fmt.Errorf("turns 为空")
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
				return sc, fmt.Errorf("turns[%d]: gap_hours 不能为负", i)
			}
			n++
		}
		if n != 1 {
			return sc, fmt.Errorf("turns[%d]: 每步只能是 say、compact、gap_hours 之一", i)
		}
		sc.Turns[i].Say = strings.TrimSpace(st.Say)
	}
	if says == 0 {
		return sc, fmt.Errorf("脚本里没有任何一句 say")
	}
	return sc, nil
}
