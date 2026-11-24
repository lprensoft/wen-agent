package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"wen/internal/i18n"
	"wen/internal/usage"
)

// maxUsageSettingsBody 是预算设置的请求体上限。它只有五六个标量字段，留 64KB 是
// 给手写客户端留的余量，同时挡住「一次塞几十兆进来」的请求。
const maxUsageSettingsBody = 64 << 10

// usageMonthFormat 与账本分片名的月份前缀同形。月份会被拼进分片名前缀，先在
// 这里校验，非法值绝不落到账本目录外面。
const usageMonthFormat = "2006-01"

// usageItem 是报告里的一行：合计、某一天、某个来源或某个模型。
//
// 金额按币种分开给（costs），不折成一个数：价格表里本来就可能有不同地区的账单，
// 换算要联网、会过时，界面上并列几行比一个悄悄换算过的总数诚实。
type usageItem struct {
	Key        string `json:"key"`
	Requests   int    `json:"requests"`
	Prompt     int    `json:"prompt"`
	Completion int    `json:"completion"`
	Cached     int    `json:"cached"`
	CacheWrite int    `json:"cache_write"`
	// Costs 为空表示这段区间一条定价记录都没有；键是币种，统一大写（价格表里写
	// "cny" 与预算里写 "CNY" 说的是同一个币种，不该在界面上分两行）。
	Costs          map[string]float64 `json:"costs,omitempty"`
	UnpricedTokens int                `json:"unpriced_tokens,omitempty"`
	Inexact        bool               `json:"inexact,omitempty"`
	// Overflow 表示这段区间的金额被截断过：某笔（或累加后的合计）超出 float64 的
	// 可表示范围，已饱和到 MaxFloat64。它与 inexact 是两件事——金额能被编码不代
	// 表它是对的，界面得单独说一句「已截断」。
	Overflow bool `json:"overflow,omitempty"`
	// SkippedFiles 只出现在合计上：同一个坏分片会落进多个分组，按组计数没有意义。
	SkippedFiles int `json:"skipped_files,omitempty"`
}

// usageItemOf 把一份成本合计转成报告里的一行。key 为空表示合计。
func usageItemOf(key string, c usage.CostTotals) usageItem {
	return usageItem{
		Key:            key,
		Requests:       c.Requests,
		Prompt:         c.Prompt,
		Completion:     c.Completion,
		Cached:         c.Cached,
		CacheWrite:     c.CacheWrite,
		Costs:          c.Amounts,
		UnpricedTokens: c.UnpricedTokens,
		Inexact:        c.Inexact,
		Overflow:       c.Overflow,
		SkippedFiles:   c.SkippedFiles,
	}
}

// usageReportView 是 GET /api/usage 的响应体。
//
// daily / by_source / by_model 恒为数组（空报告时是空数组而不是 null）：界面按
// 数组画表，null 会多出一条「这不是列表」的分支。
type usageReportView struct {
	Month    string      `json:"month"`
	Totals   usageItem   `json:"totals"`
	Daily    []usageItem `json:"daily"`
	BySource []usageItem `json:"by_source"`
	ByModel  []usageItem `json:"by_model"`
	// Budget 没有预算判定时整个省略（本次运行没装配预算，不是「不设限」）。
	// 它的 Month 只可能是当前自然月：预算是运行中那一份，不按查询月份回算。
	Budget *usage.BudgetStatus `json:"budget,omitempty"`
	// BudgetMonthMatches 表示 budget 的月份与本次查询的 month 相同。查历史月份时
	// 它必为 false：界面据此把预算那块标成「本月」而不是挂在这份报表上。
	BudgetMonthMatches bool           `json:"budget_month_matches"`
	Settings           usage.Settings `json:"settings"`
	// SettingsError 非空时 Settings 是默认值：读不出来不等于没设限，界面要说出来。
	SettingsError string `json:"settings_error,omitempty"`
	// SettingsStale 表示磁盘上的 usage.json 与运行中的那份预算已经不一致（有别的
	// 窗口或手改过文件）。此时页面上的设置是磁盘的新值，而限额仍按旧值判——不说出来，
	// 用户会以为刚改的上限已经在管着，实际要等保存一次或重启。
	SettingsStale bool `json:"settings_stale"`
	// Degraded 表示这份报告不完整：没有账本，或设置读不出来。
	Degraded bool `json:"degraded"`
}

// usageReport 返回某个自然月的用量与金额（默认本月）。
//
// 账本为 nil 时不报错：用量是旁路数据，接口返回空报告 + degraded，界面据此显示
// 「这次运行没有记账」而不是一条错误。
func (s *Server) usageReport(w http.ResponseWriter, r *http.Request) {
	// 用量是每次现扫账本算出来的：缓存下来只会让「刚花掉的钱」在刷新后仍不出现。
	w.Header().Set("Cache-Control", "no-store")
	month := r.URL.Query().Get("month")
	if month == "" {
		month = time.Now().In(time.Local).Format(usageMonthFormat)
	}
	if !validUsageMonth(month) {
		writeError(w, http.StatusBadRequest, i18n.T(reqLang(r), "server.err.bad_month", "月份格式应为 YYYY-MM"))
		return
	}

	view := usageReportView{
		Month:    month,
		Daily:    []usageItem{},
		BySource: []usageItem{},
		ByModel:  []usageItem{},
	}
	if s.usageSettings != "" {
		st, err := usage.LoadSettings(s.usageSettings)
		view.Settings = st
		if err != nil {
			view.SettingsError = err.Error()
			view.Degraded = true
		}
	} else {
		view.Settings = usage.DefaultSettings()
	}
	if s.usageBudget != nil {
		// 设置读得出来、账本也还接着，就说明预算的降级理由已经不在了：清掉设置侧
		// 的标记，别让一次启动时的读失败把上限挡到进程结束。账本能不能写由预算
		// 自己按重扫/记账恢复（见 usage.Budget），这里不替它断定。
		if view.SettingsError == "" && s.usage != nil {
			s.usageBudget.MarkReady()
		}
		b := s.usageBudget.Status(time.Now())
		view.Budget = &b
		view.BudgetMonthMatches = b.Month == month
		// 设置以磁盘为准（用户刚改的那份），预算以运行中那份为准：两边不一致时
		// 界面得说明「现在管的还是旧值」，否则改完上限的人会以为它立刻生效了。
		view.SettingsStale = settingsStale(view.Settings, b)
	}
	if s.usage == nil {
		// 没有账本：空合计（字段都在，数字为 0），由 degraded 交代原因。
		view.Degraded = true
		writeJSON(w, http.StatusOK, view)
		return
	}

	// 价格表可能还没配（模型配置坏了、或没填任何单价）：lookup 为 nil 时账本把
	// 全部用量记成「未定价」，token 照常合计。
	//
	// 价格在这里一次取定（快照），整份报表的合计与三份分组共用同一份：逐条现查的话，
	// 扫描期间保存一次模型就会改价，同一份报表里出现两套单价。
	var lookup usage.PriceLookup
	if s.models != nil {
		lookup = s.models.PriceSnapshot()
	}

	// 一次扫完：合计与三份分组出自同一份读结果，每条记录的金额只算一次。
	rep, err := s.usage.ReportCostOnMonth(month, lookup)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	view.Totals = usageItemOf("", rep.Total)
	view.Daily = usageItemsOf(rep.Daily)
	view.BySource = usageItemsOf(rep.BySource)
	// 金额挂在模型上，而同一个模型名可能出现在两家提供商下、单价不同：报告按
	// 「提供商 / 模型」拆——by_model 的 key 是「提供商 + 空格斜杠空格 + 模型」两段，
	// 界面上的金额才能与价格表一行行对上。
	view.ByModel = usageItemsOf(rep.ByModel)
	writeJSON(w, http.StatusOK, view)
}

// usageItemsOf 把一组成本分组转成报告里的行。空分组给空数组：界面按数组画表。
func usageItemsOf(groups []usage.GroupCost) []usageItem {
	items := make([]usageItem, 0, len(groups))
	for _, g := range groups {
		items = append(items, usageItemOf(g.Key, g.CostTotals))
	}
	return items
}

// decodeUsageSettings 把请求体解进 base：未知字段与 null 一律拒绝，缺省字段保持原值。
//
// 与 usage.json 的严格解析同一口径（见 usage 包的 parseSettings）：拼错的键静默
// 忽略会让人以为「限额已经设上了」，而 null 与「没写」解开之后长得一样、意思却
// 可能相反（清空限额 vs 忘了填）。base 由调用方给出——缺省字段保持的是现有值，
// 不是零值。
func decodeUsageSettings(raw []byte, base usage.Settings) (usage.Settings, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return usage.Settings{}, err
	}
	if fields == nil {
		return usage.Settings{}, fmt.Errorf("usage settings: want a JSON object")
	}
	for k, v := range fields {
		if string(bytes.TrimSpace(v)) == "null" {
			return usage.Settings{}, fmt.Errorf("usage settings: %q must not be null", k)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&base); err != nil {
		return usage.Settings{}, err
	}
	return base, nil
}

// saveUsageSettings 保存预算设置，并让运行中的那份预算立即生效。
//
// 先落盘再热更新：热更新失败时文件里已经是用户确认过的值，下一次启动会照它来；
// 反过来先改内存再落盘，一次写失败就会让「界面显示已保存」与磁盘上的内容不一致。
//
// 整段「读底稿 → 合并 → 落盘 → 读回 → 热更新」持锁串行：界面只提交改过的字段，
// 底稿必须是别人刚写完的那一份，否则并发保存会互相把对方设的项按缺省值清掉。
func (s *Server) saveUsageSettings(w http.ResponseWriter, r *http.Request) {
	if s.usageSettings == "" {
		writeError(w, http.StatusInternalServerError, i18n.T(reqLang(r), "server.err.no_usage_settings", "预算设置路径不可用，无法保存"))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUsageSettingsBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, i18n.T(reqLang(r), "server.err.bad_body", "请求体解析失败或超过大小上限"))
		return
	}
	// 请求体先读完再拿锁：读网络与等写盘不该互相拖住。
	s.usageSettingsMu.Lock()
	defer s.usageSettingsMu.Unlock()

	var st usage.Settings
	// 以现有设置为底稿：界面只提交改过的字段，漏一项就退回 0 会把用户的限额悄悄
	// 清掉。文件本身读不出来（手写坏了）时从默认值起——这次保存本来就是要整份覆盖它。
	// 但版本比本程序新时不覆盖：那份文件里可能有不认识的字段，覆盖一次就抹平了。
	base, err := usage.LoadSettings(s.usageSettings)
	if err != nil {
		var tooNew *usage.SettingsTooNewError
		if errors.As(err, &tooNew) {
			writeError(w, http.StatusConflict, i18n.Tf(reqLang(r), "server.err.settings_too_new",
				"usage.json 的版本（%d）比本程序认识的新，请先升级程序再改预算设置", tooNew.Found))
			return
		}
		base = usage.DefaultSettings()
	}
	st, err = decodeUsageSettings(raw, base)
	if err != nil {
		writeError(w, http.StatusBadRequest, i18n.Tf(reqLang(r), "server.err.bad_usage_settings", "预算设置无效：%s", err.Error()))
		return
	}
	if err := usage.SaveSettings(s.usageSettings, st); err != nil {
		// SaveSettings 的失败几乎都是校验不过（负限额、未知 over），按 400 报，
		// 让界面把原话给用户看。金额上限的两条边界单独翻：它们说的是「填了等于
		// 没设」，需要一句解释，而不是一句 validate 失败。
		switch {
		case errors.Is(err, usage.ErrCostTooSmall):
			writeError(w, http.StatusBadRequest, i18n.T(reqLang(r), "usage.err.cost_too_small",
				"金额上限太小：至少要能表示成 0.000000001，低于这个数等于没有设上限"))
			return
		case errors.Is(err, usage.ErrCostTooLarge):
			writeError(w, http.StatusBadRequest, i18n.T(reqLang(r), "usage.err.cost_too_large",
				"金额上限过大：超出可精确表示的范围，请填一个更小的数"))
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	loaded, err := usage.LoadSettings(s.usageSettings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// budget 为 nil 时（评测装配形态）只保存：没有判定对象可热更新。
	if s.usageBudget != nil {
		// 刚读出来的一份合法设置就是「设置能读了」的证据：清掉启动时留下的降级，
		// 否则用户修好了配置，上限还要等下次重启才恢复。
		s.usageBudget.MarkReady()
		if err := s.usageBudget.SetSettings(loaded); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	resp := map[string]any{"settings": loaded}
	if s.usageBudget != nil {
		resp["budget"] = s.usageBudget.Status(time.Now())
	}
	writeJSON(w, http.StatusOK, resp)
}

// settingsStale 回答「磁盘上的设置与运行中的预算是否已经分叉」。
//
// 只比这五项：其余字段（version）不参与判定，而且 LoadSettings 与 normalize 都已经
// 把币种、over 这类归一化过，直接比不会因大小写差异误报。settings_error 那一档也
// 照比——文件读不出来时磁盘上的值确实是默认值，而预算还按旧值判，两句话都对。
func settingsStale(s usage.Settings, b usage.BudgetStatus) bool {
	return s.MonthlyTokens != b.LimitTokens || s.MonthlyCost != b.LimitCost ||
		s.Currency != b.Currency || s.WarnPercent != b.WarnPercent || s.Over != b.Over
}

// validUsageMonth 确认 month 是可以直接当分片名前缀用的自然月。解析成功还不够：
// Parse 对 "2026-1" 这类写法比较宽容，而它不能当成 2026 年 1 月用。
func validUsageMonth(month string) bool {
	t, err := time.Parse(usageMonthFormat, month)
	return err == nil && t.Format(usageMonthFormat) == month
}
