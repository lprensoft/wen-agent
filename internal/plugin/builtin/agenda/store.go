// Package agenda 提供日程的系统插件：角色在对话之外有自己的一天——早上醒来排一张
// 两到四件事的表，到点去做、做完回来带着一句经历，遇到变故会改、会挪、会跟对方商量；
// 表里的「和谁」来自人物库里有名有姓的人。
//
// 它与相邻插件的分工：scheduler 管「到点自动跑一轮」的提醒机制，这里管状态记录——
// 一天的表、定在某天某时的约定，以及答应下来但没有时刻的事（那份台账见 Promise）；
// presence 记此刻在哪、在做什么的定格，这里给出的是一天的粗粒度安排，活动开始与结束
// 的轮次里由模型顺手更新现场；memory 按时间线记过去，这里只记今天与将来——结束轮次
// 写下的经历以助手消息落进会话，时间线自然收进去，本插件不另写记忆。
//
// 全部只用现有契约（StateDir / Sessions / RunTurn / TurnPrompt / TurnObserver /
// Stoppable / Notice / StatusReporter），核心不知道「日程」这回事；与心跳、消息通道的
// 衔接走 internal/cue（开口理由）与 internal/availability（忙碌状态），两者同级。
package agenda

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wen/internal/prompt"
)

// 字段上限。表与约定随每轮注入，单条必须有确定上界；超长报错让模型压缩后重试，
// 不悄悄截断——被截掉的部分模型不知道丢了。
const (
	maxTitleRunes   = 30
	maxPlaceRunes   = 30
	maxOutcomeRunes = 80
	maxNoteRunes    = 60
	maxWith         = 4
	hardMaxItems    = 6 // 项数的硬上限，配置项 max_items 不能超过它
	maxCommitments  = 50
	maxPromises     = 50
	maxPromiseShown = 6 // 每轮注入的「答应过的事」条数上限，超出压成条数
	// promiseGraceDays 是到期日过后仍然注入的天数。到期当天不算数：对方可能晚上才
	// 兑现。给一天宽限，之后自动转为没做成并停止注入——一条永远挂着的待办，
	// 就是换了个载体的同一种病。
	promiseGraceDays = 1
)

// 答应过的事的状态。dropped 是「不作数了」：说好的事被双方取消，与没做成不同。
const (
	promisePending = "pending"
	promiseDone    = "done"
	promiseMissed  = "missed"
	promiseDropped = "dropped"
)

// 谁答应的。两个方向都要记，且必须分得开——混在一起模型会去催对方做它自己答应的事。
const (
	promiseBySelf = "self"
	promiseByUser = "user"
)

// 项的状态。
const (
	statusPlanned   = "planned"
	statusOngoing   = "ongoing"
	statusDone      = "done"
	statusSkipped   = "skipped"
	statusDeferred  = "deferred"
	statusCancelled = "cancelled"
)

// flex 三档：能不能挪。
//
// 这两组值都**会原样落盘**，因此走 prompt.Enum：中文是规范形式，Schema 按语言给出
// 候选项，模型报回来的值先换回中文再存。表已经排好之后换一门语言，读出来的仍是
// 同一批值，跳过、排序、可挪判定都不受影响。
const (
	flexMovable = "可挪"
	flexTry     = "尽量守"
	flexFixed   = "不能动"
)

var flexLevels = prompt.NewEnum(
	prompt.Text("prompt.agenda.flex.movable", flexMovable),
	prompt.Text("prompt.agenda.flex.try", flexTry),
	prompt.Text("prompt.agenda.flex.fixed", flexFixed),
)

// busy 三档：做这件事时能不能回消息，与 availability 的档位同一套词。
const (
	busyLight = "轻忙"
	busyHeavy = "重忙"
	busyNone  = "不回"

	defaultBusy = busyLight
)

var busyLevels = prompt.NewEnum(
	prompt.Text("prompt.agenda.busy.light", busyLight),
	prompt.Text("prompt.agenda.busy.heavy", busyHeavy),
	prompt.Text("prompt.agenda.busy.none", busyNone),
)

// Item 是当天表里的一项。
type Item struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Start          string   `json:"start"` // HH:MM 本地时间
	End            string   `json:"end"`   // HH:MM，早于 Start 视为次日
	Place          string   `json:"place,omitempty"`
	With           []string `json:"with,omitempty"`
	WithUser       bool     `json:"with_user,omitempty"`
	FromCommitment string   `json:"from_commitment,omitempty"`
	Flex           string   `json:"flex"`
	Busy           string   `json:"busy"`
	Status         string   `json:"status"`
	Outcome        string   `json:"outcome,omitempty"`
	// StartFired / EndFired 是开始 / 结束轮次**已派发**的时刻：记「上次发生」不记倒计时，
	// 派发前落盘，重启不重发。SoonFired 是「快到了」的开口理由投出的时刻，同理。
	StartFired time.Time `json:"start_fired,omitempty"`
	EndFired   time.Time `json:"end_fired,omitempty"`
	SoonFired  time.Time `json:"soon_fired,omitempty"`
}

// terminal 报告该项是否已到终态（做完、跳过、延期、取消）。
func (it *Item) terminal() bool {
	switch it.Status {
	case statusDone, statusSkipped, statusDeferred, statusCancelled:
		return true
	}
	return false
}

// Attempts 记录某一天规划轮次已尝试的次数：失败两次就到明天再说。
type Attempts struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// Plan 是一天的表。文件里始终只有最近的一张：过去的日子由时间线记经历，不在这里归档。
type Plan struct {
	Date      string    `json:"date"` // 逻辑日期 YYYY-MM-DD
	Weekday   string    `json:"weekday"`
	PlannedAt time.Time `json:"planned_at"`
	Note      string    `json:"note,omitempty"`
	Items     []Item    `json:"items"`
	// Attempts 与 Date 无关：它记的是「今天」的规划尝试次数，而 Date 可能还是昨天。
	Attempts Attempts `json:"plan_attempts"`
}

// item 按 id 查找，找不到返回 nil。
func (pl *Plan) item(id string) *Item {
	for i := range pl.Items {
		if pl.Items[i].ID == id {
			return &pl.Items[i]
		}
	}
	return nil
}

// Promise 是一条答应过的事：没有时刻，只有一个到期日与兑现状态。
//
// 它与 Commitment 的区别不是粗细，是形状：约定「明天下午三点见面」占时间、要排进表、
// 会提前提醒、做的时候人不在；而「明天给你带两个菜」不占时间、不排表、不该提醒，
// 它只需要被记住，然后在某一天被了结。硬塞进 Commitment 得给那三条路径各加一个分支。
//
// 存在的理由是这类话此前无处可去：不是记忆（单次安排按约定归日程）、不是约定（没时刻）、
// 也不是当天表上的项，于是只以对话文本的形式活着——而对话文本既没有时间锚，也没有
// 完成状态，于是每晚被重说一遍，措辞永远是「明天」。
type Promise struct {
	ID      string    `json:"id"`
	Date    string    `json:"date"` // 到期日 YYYY-MM-DD：答应「明天」时就是那天
	Title   string    `json:"title"`
	By      string    `json:"by"` // self（角色答应的）/ user（对方答应的）
	Note    string    `json:"note,omitempty"`
	Status  string    `json:"status"`
	Outcome string    `json:"outcome,omitempty"` // 了结时的一句话
	Created time.Time `json:"created"`
	Settled time.Time `json:"settled,omitempty"`
}

// settled 报告这条是否已经了结。
func (pr *Promise) settled() bool { return pr.Status != promisePending }

// promisesFile 是台账文件的形状，取号规则与约定相同（见 commitmentsFile）。
type promisesFile struct {
	NextID int       `json:"next_id"`
	Items  []Promise `json:"items"`
}

// Commitment 是一条未来约定：定在某天某时的事。
type Commitment struct {
	ID       string    `json:"id"`
	Date     string    `json:"date"`  // YYYY-MM-DD
	Start    string    `json:"start"` // HH:MM
	End      string    `json:"end,omitempty"`
	Title    string    `json:"title"`
	With     []string  `json:"with,omitempty"`
	WithUser bool      `json:"with_user,omitempty"`
	Place    string    `json:"place,omitempty"`
	Note     string    `json:"note,omitempty"`
	Flex     string    `json:"flex"`
	Created  time.Time `json:"created"`
	Planned  bool      `json:"planned,omitempty"` // 已排进当天的表
}

// commitmentsFile 是约定文件的形状。带一个自增序号：约定会被取消、被清理，
// 按「现存最大号 + 1」取号会让新约定顶替旧号，模型在同一轮对话里还记着旧的那条。
type commitmentsFile struct {
	NextID int          `json:"next_id"`
	Items  []Commitment `json:"items"`
}

// Store 管理一个可见域的表与约定：两个 JSON 文件，每次操作重新读取不做缓存
// （文件可能被用户在进程外修改，体量又小）。写入在锁内同步完成——不起 goroutine 写，
// 就不存在晚到的旧快照盖掉新进展的问题。
type Store struct {
	mu sync.Mutex
	// lang 是进程语言。这个库的错误会经工具返回给模型，因此要跟着语言走。
	lang string
	dir  string
}

// NewStore 建立指向 dir 的日程库。
func NewStore(lang, dir string) *Store { return &Store{lang: lang, dir: dir} }

func (s *Store) planPath() string        { return filepath.Join(s.dir, "plan.json") }
func (s *Store) commitmentsPath() string { return filepath.Join(s.dir, "commitments.json") }
func (s *Store) promisesPath() string    { return filepath.Join(s.dir, "promises.json") }

// LoadPlan 返回当前的表；文件不存在时返回零值（Date 为空）。
func (s *Store) LoadPlan() (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadPlan()
}

func (s *Store) loadPlan() (Plan, error) {
	var pl Plan
	raw, err := os.ReadFile(s.planPath())
	if err != nil {
		if os.IsNotExist(err) {
			return pl, nil
		}
		return pl, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.store.plan_read_failed", "读取日程表失败: %w"), err)
	}
	if err := json.Unmarshal(raw, &pl); err != nil {
		return pl, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.store.plan_corrupt", "日程表文件损坏: %w"), err)
	}
	return pl, nil
}

// SavePlan 整张覆盖。
func (s *Store) SavePlan(pl Plan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savePlan(pl)
}

func (s *Store) savePlan(pl Plan) error {
	if pl.Items == nil {
		pl.Items = []Item{}
	}
	return s.writeJSON(s.planPath(), pl)
}

// UpdatePlan 在锁内读-改-写：mutate 返回 false 表示没有改动、不落盘。
func (s *Store) UpdatePlan(mutate func(pl *Plan) (bool, error)) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl, err := s.loadPlan()
	if err != nil {
		return pl, err
	}
	changed, err := mutate(&pl)
	if err != nil {
		return pl, err
	}
	if !changed {
		return pl, nil
	}
	return pl, s.savePlan(pl)
}

// LoadCommitments 返回全部约定，按记录顺序。
func (s *Store) LoadCommitments() ([]Commitment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadCommitments()
	return f.Items, err
}

func (s *Store) loadCommitments() (commitmentsFile, error) {
	var f commitmentsFile
	raw, err := os.ReadFile(s.commitmentsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.store.commit_read_failed", "读取约定失败: %w"), err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.store.commit_corrupt", "约定文件损坏: %w"), err)
	}
	return f, nil
}

// UpdateCommitments 在锁内读-改-写约定列表。mutate 可经 next 取一个新 id。
func (s *Store) UpdateCommitments(mutate func(cs *[]Commitment, next func() string) (bool, error)) ([]Commitment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadCommitments()
	if err != nil {
		return nil, err
	}
	if f.NextID <= 0 {
		f.NextID = 1
		for _, c := range f.Items { // 兼容手写的文件：从现存最大号往后取
			var n int
			if _, err := fmt.Sscanf(c.ID, "c%d", &n); err == nil && n >= f.NextID {
				f.NextID = n + 1
			}
		}
	}
	next := func() string {
		id := fmt.Sprintf("c%d", f.NextID)
		f.NextID++
		return id
	}
	changed, err := mutate(&f.Items, next)
	if err != nil {
		return nil, err
	}
	if !changed {
		return f.Items, nil
	}
	if f.Items == nil {
		f.Items = []Commitment{}
	}
	return f.Items, s.writeJSON(s.commitmentsPath(), f)
}

// LoadPromises 返回全部答应过的事，按记录顺序。
func (s *Store) LoadPromises() ([]Promise, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadPromises()
	return f.Items, err
}

func (s *Store) loadPromises() (promisesFile, error) {
	var f promisesFile
	raw, err := os.ReadFile(s.promisesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.store.promise_read_failed", "读取答应过的事失败: %w"), err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf(prompt.T(s.lang, "prompt.agenda.store.promise_corrupt", "答应过的事文件损坏: %w"), err)
	}
	return f, nil
}

// UpdatePromises 在锁内读-改-写台账。mutate 可经 next 取一个新 id。
func (s *Store) UpdatePromises(mutate func(ps *[]Promise, next func() string) (bool, error)) ([]Promise, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadPromises()
	if err != nil {
		return nil, err
	}
	if f.NextID <= 0 {
		f.NextID = 1
		for _, pr := range f.Items { // 兼容手写的文件：从现存最大号往后取
			var n int
			if _, err := fmt.Sscanf(pr.ID, "p%d", &n); err == nil && n >= f.NextID {
				f.NextID = n + 1
			}
		}
	}
	next := func() string {
		id := fmt.Sprintf("p%d", f.NextID)
		f.NextID++
		return id
	}
	changed, err := mutate(&f.Items, next)
	if err != nil {
		return nil, err
	}
	if !changed {
		return f.Items, nil
	}
	if f.Items == nil {
		f.Items = []Promise{}
	}
	return f.Items, s.writeJSON(s.promisesPath(), f)
}

// writeJSON 原子写回（tmp + rename），权限 0600：日程属于对话内容的一部分。
func (s *Store) writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.agenda.store.save_failed", "保存日程失败: %w"), err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.agenda.store.mkdir_failed", "创建日程目录失败: %w"), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.agenda.store.save_failed", "保存日程失败: %w"), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.agenda.store.save_failed", "保存日程失败: %w"), err)
	}
	return nil
}

// ---------- 日期与时刻 ----------

const dateLayout = "2006-01-02"

// weekdaysZH 是七个星期名，一行一个，从周日排起。
//
// 整表一个键而不是七个：七个键要靠拼串取用，而拼出来的键静态扫不到，覆盖检查就
// 看不见它们。核心另有一份（`星期日` 那套措辞）——两处措辞本来就不同，合并会改掉
// 中文用户看见的字，不合并的代价只是英文那边两个键给出同样的译文。
const weekdaysZH = "周日\n周一\n周二\n周三\n周四\n周五\n周六"

func weekdayName(lang string, t time.Time) string {
	names := strings.Split(prompt.T(lang, "prompt.agenda.weekdays", weekdaysZH), "\n")
	if i := int(t.Weekday()); i < len(names) {
		return names[i]
	}
	return t.Weekday().String()
}

// logicalDay 返回 now 所属的逻辑日期（零点时刻）：day_start_hour 之前算前一天——
// 凌晨一点还在聊天不算新的一天。
func logicalDay(now time.Time, dayStartHour int) time.Time {
	y, m, d := now.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	if now.Hour() < dayStartHour {
		day = day.AddDate(0, 0, -1)
	}
	return day
}

// parseHHMM 解析 "HH:MM"，返回当天的分钟数。
func parseHHMM(lang, s string) (int, error) {
	s = strings.TrimSpace(s)
	var hh, mm int
	if _, err := fmt.Sscanf(s, "%d:%d", &hh, &mm); err != nil || len(s) < 4 || len(s) > 5 {
		return 0, fmt.Errorf(prompt.T(lang, "prompt.agenda.result.bad_time",
			"时间 %q 格式应为 HH:MM，例如 14:30"), s)
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf(prompt.T(lang, "prompt.agenda.result.bad_time_value", "时间 %q 不合法"), s)
	}
	return hh*60 + mm, nil
}

// normHHMM 把时间规整成两位数的 "HH:MM"。
func normHHMM(lang, s string) (string, error) {
	m, err := parseHHMM(lang, s)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%02d:%02d", m/60, m%60), nil
}

// parseDate 解析 "YYYY-MM-DD" 为本地零点。
func parseDate(lang, s string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation(dateLayout, strings.TrimSpace(s), loc)
	if err != nil {
		return time.Time{}, fmt.Errorf(prompt.T(lang, "prompt.agenda.result.bad_date",
			"日期 %q 格式应为 YYYY-MM-DD"), s)
	}
	return t, nil
}

// absTime 把一张表上的 HH:MM 换算成绝对时刻。一天从 day_start_hour 起算，在此之前
// 的时刻属于次日凌晨（表上写 01:00 的项是这一天将尽的凌晨一点，不是排表之前的那个
// 凌晨）；end 不晚于 start 时视为跨到次日。
func absTime(day time.Time, hhmm string, dayStartHour int) time.Time {
	// 这里的入参是自己刚落盘的规整值，解不开只可能是文件被手改坏了；语言传空
	// ——错误当场丢弃，不会到模型跟前。
	m, err := parseHHMM("", hhmm)
	if err != nil {
		return time.Time{}
	}
	t := day.Add(time.Duration(m) * time.Minute)
	if m < dayStartHour*60 {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// span 返回一项的起止绝对时刻。
func (it *Item) span(day time.Time, dayStartHour int) (start, end time.Time) {
	start = absTime(day, it.Start, dayStartHour)
	end = absTime(day, it.End, dayStartHour)
	if !end.After(start) {
		end = end.AddDate(0, 0, 1)
	}
	return start, end
}

// ---------- 校验 ----------

func checkRunes(lang, v, what string, max int) error {
	if n := len([]rune(v)); n > max {
		return fmt.Errorf(prompt.T(lang, "prompt.agenda.result.field_long",
			"%s过长（%d 字，上限 %d 字），请压缩后重试"), what, n, max)
	}
	return nil
}

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// canonFlex / canonBusy 把模型报来的任意语言的字面量换回中文规范值。
// 第二个返回值为 false 表示不在表里，按参数非法处理——不猜一个最接近的。
func canonFlex(f string) (string, bool) { return flexLevels.Canonical(f) }
func canonBusy(b string) (string, bool) { return busyLevels.Canonical(b) }

// sortCommitments 按日期与开始时刻排序（稳定）。
func sortCommitments(cs []Commitment) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Date != cs[j].Date {
			return cs[i].Date < cs[j].Date
		}
		return cs[i].Start < cs[j].Start
	})
}

// sortPromises 按到期日排，同一天按记录顺序（SliceStable）。
func sortPromises(ps []Promise) {
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Date < ps[j].Date })
}

// countPending 数还没了结的条数。上限只卡未了结的：已了结的留着供 list_promises
// 回头查，不该因为历史攒多了就拒绝记新的。
func countPending(ps []Promise) int {
	n := 0
	for i := range ps {
		if !ps[i].settled() {
			n++
		}
	}
	return n
}

// promiseByCN 把方向写成人话。两个方向必须在字面上分得开——
// 混在一起模型会去催对方做它自己答应的事。
func promiseByCN(lang, by string) string {
	if by == promiseByUser {
		return prompt.T(lang, "prompt.agenda.promise.by_user", "对方答应：")
	}
	return prompt.T(lang, "prompt.agenda.promise.by_self", "你答应：")
}

// promiseByShort 只给称呼，用在句子里。
func promiseByShort(lang, by string) string {
	if by == promiseByUser {
		return prompt.T(lang, "prompt.agenda.promise.who_user", "对方")
	}
	return prompt.T(lang, "prompt.agenda.promise.who_self", "你")
}

// promiseResultCN 把了结方式写成人话。
func promiseResultCN(lang, result string) string {
	switch result {
	case promiseDone:
		return prompt.T(lang, "prompt.agenda.promise.result_done", "做到了")
	case promiseMissed:
		return prompt.T(lang, "prompt.agenda.promise.result_missed", "没做成")
	case promiseDropped:
		return prompt.T(lang, "prompt.agenda.promise.result_dropped", "不作数了")
	}
	return result
}

// fmtDateCN 把 YYYY-MM-DD 写成「8 月 23 日（周日）」。
func fmtDateCN(lang, date string, loc *time.Location) string {
	t, err := parseDate(lang, date, loc)
	if err != nil {
		return date
	}
	return prompt.Tf(lang, "prompt.agenda.date.long", "%d 月 %d 日（%s）",
		int(t.Month()), t.Day(), weekdayName(lang, t))
}

// fmtDateShort 把 YYYY-MM-DD 写成「08-23（周日）」。
func fmtDateShort(lang, date string, loc *time.Location) string {
	t, err := parseDate(lang, date, loc)
	if err != nil {
		return date
	}
	return prompt.Tf(lang, "prompt.agenda.date.short", "%s（%s）",
		t.Format("01-02"), weekdayName(lang, t))
}

// humanMinutes 把时长写成「N 分钟 / N 小时 / N 小时 N 分钟」。
//
// 不走 prompt.Duration：那一份只给一个单位的精度（一小时半说成「1 小时」），
// 而日程要报的正是「已进行 1 小时 20 分钟」这种分钟级的量。
func humanMinutes(lang string, d time.Duration) string {
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins < 0 {
		mins = 0
	}
	if mins < 60 {
		return prompt.Tf(lang, "prompt.agenda.dur.minutes", "%d 分钟", mins)
	}
	h, m := mins/60, mins%60
	if m == 0 {
		return prompt.Tf(lang, "prompt.agenda.dur.hours", "%d 小时", h)
	}
	return prompt.Tf(lang, "prompt.agenda.dur.hours_minutes", "%d 小时 %d 分钟", h, m)
}
