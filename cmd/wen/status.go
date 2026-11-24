package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"wen/internal/config"
	"wen/internal/modelcfg"
	"wen/internal/runlock"
	"wen/internal/statustext"
	"wen/internal/usage"
	"wen/internal/version"
)

// runStatus 打印当前配置与运行状态。不进交互，便于 ssh 一条命令看清楚现状。
func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	configPath := fs.String("c", "", tr("cli.flag.config", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）"))
	_ = fs.Parse(args)

	path := config.ResolvePath(*configPath)
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf(tr("cli.err.load_config", "加载配置失败: %w"), err)
	}
	adoptLang(cfg)
	b, err := openBackend(cfg)
	if err != nil {
		return err
	}

	fmt.Printf("Wen Agent %s\n", version.Version)
	if _, err := os.Stat(path); err == nil {
		fmt.Print(trf("cli.status.config_file", "配置文件  %s\n", path))
	} else {
		fmt.Print(tr("cli.status.no_config_file", "配置文件  无（全部使用默认值）\n"))
	}
	fmt.Print(trf("cli.status.config_dir", "配置目录  %s\n", cfg.BaseDir))
	fmt.Print(trf("cli.status.lang", "界面语言  %s\n", langSummary(cfg)))

	info, state := runlock.ReadState(cfg.BaseDir)
	switch {
	case state == runlock.StateRunning && reachable(info.Addr):
		fmt.Print(trf("cli.status.running", "运行状态  运行中 · 监听 %s · PID %d · 版本 %s\n", info.Addr, info.PID, info.Version))
	case state == runlock.StateRunning:
		fmt.Print(trf("cli.status.unreachable", "运行状态  已登记（%s · PID %d）但接口没有应答\n", info.Addr, info.PID))
	case state == runlock.StateUnknown:
		fmt.Print(tr("cli.status.unverified", "运行状态  无法核实（登记存在，但判不出它是不是这个配置目录的实例；可能是权限不足，或实例由另一个账号运行）\n"))
	default:
		fmt.Print(trf("cli.status.stopped", "运行状态  未运行（配置里的监听地址为 %s:%d）\n", cfg.Server.Host, cfg.Server.Port))
	}

	if auth, err := b.loadAuth(); err == nil {
		fmt.Print(trf("cli.status.auth", "访问控制  %s\n", describeAuth(auth)))
	}

	if doc, err := b.loadModels(); err == nil {
		if doc.Current.Provider == "" {
			fmt.Print(tr("cli.status.no_model", "当前模型  未配置（运行 wen onboard）\n"))
		} else if p, ok := findProviderView(doc, doc.Current.Provider); ok && !p.HasAPIKey && modelcfg.RequiresKey(p.BaseURL) {
			// 选中了但缺密钥，服务起不来——照常报一行「当前模型」会让人以为一切正常
			fmt.Print(trf("cli.status.model_no_key", "当前模型  %s / %s（缺 API Key，无法使用）\n", doc.Current.Provider, doc.Current.Model))
		} else {
			fmt.Print(trf("cli.status.model", "当前模型  %s / %s\n", doc.Current.Provider, doc.Current.Model))
		}
	}

	if line := statusUsageLine(cfg, time.Now()); line != "" {
		fmt.Print(line + "\n")
	}

	list, err := b.listPlugins()
	if err != nil {
		return err
	}
	var on, off []string
	for _, p := range list {
		if p.Enabled {
			on = append(on, p.Name)
		} else {
			off = append(off, p.Name)
		}
	}
	fmt.Print(trf("cli.status.enabled", "已启用插件（%d）%s\n", len(on), wrapNames(on)))
	fmt.Print(trf("cli.status.disabled", "已禁用插件（%d）%s\n", len(off), wrapNames(off)))
	// 数据来源只说「读的是哪儿」：运行状态已经由上面那一行交代，这里再按后端类型
	// 报「服务未运行」会与它打架（身份没核实、或已登记但没应答时也是离线读本地）。
	source := tr("cli.status.source_local", "本地配置文件")
	if _, online := b.(*onlineBackend); online {
		source = tr("cli.status.source_online", "运行中的服务")
	}
	fmt.Print(trf("cli.status.source", "\n数据来源  %s\n", source))
	return nil
}

// statusUsageLine 给出「本月用量」那一行，以及它后面按需追加的预算段。
//
// 直接从文件读而不经服务的接口：服务没跑也要能看，而用量是只追加的旁路数据，
// 复用 /api/status 只会让这一行在离线时消失。没有记录、没设预算、也没记账失败时
// 返回空串——一行「0 次」不占版面；但设了预算就不能因为本月还没花而把预算藏掉，
// 那会让用户以为限额没生效（记账失败同理，见 statusBudgetSuffix）。
func statusUsageLine(cfg *config.Config, now time.Time) string {
	month := now.In(time.Local).Format("2006-01")
	// 价格表读不出来时只报 token：金额是旁路信息，不该让这一行整个消失。
	lookup, _ := usagePriceLookup(cfg)
	var total usage.CostTotals
	ledger, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err == nil {
		if sum, err := ledger.SummaryCostOnMonth(month, usage.GroupByKind, lookup); err == nil {
			total = sum.Total
		}
	}

	line := ""
	if total.Requests > 0 {
		line = trf("cli.status.usage", "本月用量  %d 次 · 输入 %s · 输出 %s tokens",
			total.Requests, statustext.Comma(total.Prompt), statustext.Comma(total.Completion))
		line += statusCostSuffix(total)
	}

	// 设置读不出来时标降级，而不是按默认值（不设限）把它悄悄藏掉：预算失效最坏的
	// 形态就是用户以为限额在生效、实际一分没限。
	settings, settingsErr := usage.LoadSettings(usageSettingsFor(cfg, runtimeOverrides{}))
	budget := usage.NewBudget(settings, ledger, lookup, now)
	if settingsErr != nil {
		budget.MarkDegraded()
	}
	st := budget.Status(now)
	suffix := statusBudgetSuffix(st)
	if suffix == "" {
		return line
	}
	if line == "" {
		line = tr("cli.status.usage.none", "本月用量  暂无记录")
	}
	return line + suffix
}

// statusBudgetSuffix 给出用量行后面的预算段，没有可说的返回空串。
//
// 不设限时一般什么都不说（默认状态），但账本写不进去时要破例：本月合计偏少与
// 上限定没定无关，藏起来就会让人把偏低的数字当成真实用量。
func statusBudgetSuffix(st usage.BudgetStatus) string {
	if !st.Enabled && !st.Degraded && !st.WriteFailed {
		return ""
	}
	if st.Degraded {
		return tr("cli.status.usage.budget_degraded", " · 本月预算不可用（账本或设置读不出来）")
	}
	if !st.Enabled {
		return trf("cli.status.usage.budget", " · 本月预算 %s",
			tr("cli.usage.budget.write_failed_only", "未设置（账本写入失败，统计可能不完整）"))
	}
	// token 段与金额段并列，限额为 0 的维度整段省略：只设钱的上限是正常配置。
	var parts []string
	if st.LimitTokens > 0 {
		parts = append(parts, trf("cli.usage.budget.line", "%s / %s tokens（%s%%）",
			statustext.Comma64(st.SpentTokens), statustext.Comma64(st.LimitTokens),
			statustext.Pct64(st.SpentTokens, st.LimitTokens)))
	}
	if cost := budgetCostPart(st); cost != "" {
		parts = append(parts, cost)
	}
	// 设了金额上限却没有价格表：那一段永远不会生效，必须说出来。
	if st.LimitCost > 0 && !st.CostReady {
		parts = append(parts, tr("cli.usage.budget.no_price", "（缺少价格，金额上限未生效）"))
	} else if st.LimitCost > 0 && st.CostUnpriced {
		// 价格表在，本月用到的模型却一个都没定价：同样等于没有金额上限。
		parts = append(parts, tr("cli.usage.budget.unpriced_usage", "（本月有用量没有价格，金额上限未生效）"))
	}
	// 别的币种不计入本币上限；坏分片让合计偏少。两者都会让上面的数字偏离真相。
	if st.LimitCost > 0 && st.ForeignCost {
		parts = append(parts, trf("cli.usage.budget.foreign", "（有其他币种的花费未计入 %s 上限）", st.Currency))
	}
	if st.SkippedFiles > 0 {
		parts = append(parts, trf("cli.usage.budget.skipped", "（%d 个账本分片读不出来，合计可能偏少）", st.SkippedFiles))
	}
	// 记账失败只让统计偏少，不改变拦截：说出来即可。
	if st.WriteFailed {
		parts = append(parts, tr("cli.usage.budget.write_failed", "（账本写入失败，本月统计可能不完整）"))
	}
	out := trf("cli.status.usage.budget", " · 本月预算 %s", strings.Join(parts, " · "))
	// 超限的拦截范围取决于 over：block_all 连前台一起停，只说「后台」是低报。
	switch {
	case st.Exceeded && st.Over == usage.OverBlockAll:
		out += tr("cli.status.usage.budget_exceeded_all", "（已达上限，所有请求已暂停）")
	case st.Exceeded:
		out += tr("cli.status.usage.budget_exceeded", "（已达上限，后台任务已暂停）")
	case st.Warn:
		out += tr("cli.status.usage.budget_warn", "（接近上限）")
	}
	return out
}

// statusCostSuffix 给出用量行后面的金额与未定价提示，没有可报的就返回空串。
//
// 与 wen usage 的合计行同一套口径，只是这里更短：一行状态本来就该短。多币种并列；
// 「未定价」要说出来——那部分 token 没折成钱，只看金额会以为一分没花。
func statusCostSuffix(c usage.CostTotals) string {
	out := ""
	if len(c.Amounts) > 0 {
		curs := make([]string, 0, len(c.Amounts))
		for cur := range c.Amounts {
			curs = append(curs, cur)
		}
		sort.Strings(curs)
		amounts := make([]string, 0, len(curs))
		for _, cur := range curs {
			amounts = append(amounts, cur+" "+statustext.Money(c.Amounts[cur]))
		}
		out = trf("cli.status.usage.cost", " · 估算 %s", strings.Join(amounts, " · "))
	}
	if c.UnpricedTokens > 0 {
		out += trf("cli.status.usage.unpriced", " · 未定价 %s tokens", statustext.Comma(c.UnpricedTokens))
	}
	return out
}

// statusBudgetCost 已并入 usage.go 的 budgetCostPart：两条命令的金额段口径必须同一份，
// 否则同一个数字会在这里说「金额为估算」、在那里不说。

func wrapNames(names []string) string {
	if len(names) == 0 {
		return tr("cli.status.none", "  （无）")
	}
	return "\n  " + joinList(names)
}

// describeAuth 与服务端启动日志的 authSummary 说的是同一件事，
// 措辞保持一致；此处多说一句「口令来自哪里」，因为 CLI 常在排查时用。
func describeAuth(a authState) string {
	switch {
	case a.EnvManaged:
		return tr("cli.auth.env", "已设置口令（来自环境变量 WEN_AUTH_PASSWORD）")
	case !a.HasPassword:
		return tr("cli.auth.none", "未设置口令，仅本机可访问")
	case !a.TrustLoop:
		return tr("cli.auth.allLogin", "已设置口令，所有来源（含本机）都需登录")
	case a.Exposed:
		return tr("cli.auth.loopFree", "已设置口令，本机免登录、其它来源需登录")
	default:
		return tr("cli.auth.localOnly", "已设置口令，但当前只监听本机，不会被外部访问")
	}
}

func findProviderView(doc modelsDoc, name string) (providerView, bool) {
	for _, p := range doc.Providers {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return providerView{}, false
}
