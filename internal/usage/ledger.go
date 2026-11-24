// Package usage 记录模型调用的长期用量账本，并管理它的预算设置。
//
// 为什么是一份独立账本：session.Meta.LastUsage 每次请求都覆盖，只留下「最后一次」
// 的数字——一个会话跑过几十轮，能回答的也只是最后一轮的 token。而花钱的并不只有
// 用户坐在面前的轮次：心跳、定时任务这类后台轮次无人值守地反复调用模型，却不在
// 任何人的视野里。要回答「这个月用了多少、是哪个发起方在花」，需要一条只追加、
// 从不覆盖的记录：每次调用落一行到当天的文件里，随时可以离线聚合，进程重启不会
// 把已有记录冲掉，同一天的行也能事后补算。
//
// 账本按天分片（YYYY-MM-DD.jsonl），一行一条；设置是单份 JSON（路径由调用方给出，
// 通常是 <配置目录>/usage.json）。两者形状不同是因为来历不同：账本是机器写的、
// 只追加；设置是人改的，手写、可读、整份覆盖。
//
// 账本当前只覆盖三类模型调用：对话轮次、会话压缩与插件的辅助调用。它不含向量
// 调用与模型试连——前者的 API 不返回 usage，记出来的数字只能是猜的；后者是用户
// 在设置里显式点的一次探测，成本可以忽略，记进账本反而会让「这个月花了多少」
// 多出几条与使用无关的噪声。
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"wen/internal/clock"
)

// 分组的维度名。未知值由 Summary 报错，不再悄悄按某个默认维度算。
const (
	GroupByDay      = "day"
	GroupBySource   = "source"
	GroupByModel    = "model"
	GroupByKind     = "kind"
	GroupByProvider = "provider"
	// GroupByProviderModel 是「这家的这个模型」：价格挂在模型上，而同一个模型名
	// 可能出现在两家提供商下、单价不同，只看模型名会把两笔不同价的账合成一笔。
	GroupByProviderModel = "provider_model"
)

// 轮次类型。Kind 是最省事的分类法：一次调用要么是用户看得见的一轮，要么是压缩，
// 要么是插件自己发起的辅助调用。别的取值也能写进去，但这三个是核心会自己用的。
const (
	KindTurn    = "turn"
	KindCompact = "compact"
	KindHelper  = "helper"
)

// foregroundKey 是来源为空（网页前台）时的分组键。空串做键在界面上没有可读性，
// 而这里正好有一个有意义的词。
const foregroundKey = "foreground"

// unknownKey 是其余维度上空键（含只有空白）的落点。旧记录与外部写入方可能没写
// model / kind，把它们归进一个明说的桶，比在界面上留一行没有标题的数字好。
const unknownKey = "unknown"

const dayFormat = "2006-01-02"

// monthFormat 是自然月的形状，只用来校验 TotalsOnMonth / SummaryOnMonth 的参数，
// 分片名仍然是按天的。
const monthFormat = "2006-01"

// scanDaySlack 是分片名预筛的日期余量。条目 TS 的本地日期与文件名一致，但读的
// 一方可能不在写时的时区里，所以按 ±48 小时把候选日期放宽，再由逐行的 TS 过滤
// 收准——预筛只负责少开文件，不负责正确性。
const scanDaySlack = 48 * time.Hour

// dayFilePattern 只认账本自己的分片名。目录里可能还有设置文件、备份与写了一半的
// 临时文件，按名字筛比按后缀筛稳。
var dayFilePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.jsonl$`)

// maxLineBytes 是单行上限。超出的行让 scanner 整体失败，这个文件按坏文件跳过：
// 一条写坏的巨型行不该让整份账本读不出来，也不该被内存吃光。测试会临时调小它来
// 造超长行，所以它是变量而不是常量。
var maxLineBytes = 16 * 1024 * 1024

// Entry 是一次模型调用的用量。Kind 区分轮次类型，Origin 记发起方（空 = 网页前台），
// 两者合起来才能回答「后台的哪些活花了多少」。
type Entry struct {
	TS         time.Time `json:"ts"`
	Day        string    `json:"day,omitempty"`    // 写入时的当地日期（YYYY-MM-DD），分片名与按日分组都用它
	Kind       string    `json:"kind"`             // turn / compact / helper
	Origin     string    `json:"origin,omitempty"` // 发起方插件名；空 = 网页前台
	Provider   string    `json:"provider,omitempty"`
	Model      string    `json:"model,omitempty"`
	Prompt     int       `json:"prompt"`
	Completion int       `json:"completion"`
	Cached     int       `json:"cached,omitempty"`
	CacheWrite int       `json:"cache_write,omitempty"`
}

// Totals 是一段区间内的合计数。Requests 是调用次数，不是 token。
type Totals struct {
	Requests   int
	Prompt     int
	Completion int
	Cached     int
	CacheWrite int
	// SkippedFiles 是整份读不出来（打不开，或中途读错，如单行超长）而跳过的分片数。
	// 只填在区间合计上，分组里不填：同一个文件会落进多个分组，按组计数没有意义。
	SkippedFiles int
}

// GroupTotals 是一个分组键下的合计。
type GroupTotals struct {
	Key    string
	Totals Totals
}

// Summary 是全量合计加上按某个维度拆分的分组，分组按键字典序。
type Summary struct {
	Total  Totals
	Groups []GroupTotals
}

// Ledger 是一条按天分片的用量账本。单进程的追加由 mutex 串行；O_APPEND 保证追加
// 不覆盖已有内容，但跨进程并不保证单行原子——损坏行由读取端跳过。
type Ledger struct {
	dir string

	mu sync.Mutex // 只串行化本进程的追加
}

// Open 打开 dir 下的账本。dir 是 usage 目录本身（如 <配置目录>/usage），
// 不存在也可以打开——第一次 Append 时才创建，只读的调用方不会留下空目录。
func Open(dir string) (*Ledger, error) {
	if dir == "" {
		return nil, fmt.Errorf("usage: empty ledger dir")
	}
	return &Ledger{dir: dir}, nil
}

// Probe 试一次写入，确认账本目录真的能用（建得出来、写得进去）。
//
// Open 是惰性的，于是「目录建不出来」要等到第一次 Append 才发现；而在服务里那
// 意味着每个请求都失败一次、各刷一条日志。启动时探一次，失败就整场不记账，日志
// 因此只有一条。它只建目录与一个临时文件，不碰任何分片。
func (l *Ledger) Probe() error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return fmt.Errorf("create usage dir: %w", err)
	}
	err := probeWrite(l.dir)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	// Windows 上杀毒软件与索引器会短暂独占刚建出来的目录，照 openAppend 的节奏
	// 再试约 75ms。探活的代价是一次降级，比一次 Append 失败更值得多等这几毫秒。
	delay := 5 * time.Millisecond
	for i := 0; i < 4; i++ {
		time.Sleep(delay)
		delay *= 2
		if err = probeWrite(l.dir); err == nil {
			return nil
		}
	}
	return err
}

// probeWrite 建一个临时文件再删掉，走一遍真正的读写路径。
func probeWrite(dir string) error {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return fmt.Errorf("probe usage dir: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("probe usage dir: %w", err)
	}
	return nil
}

// Append 追加一条记录。TS 为零值时取当前时刻。
func (l *Ledger) Append(e Entry) error {
	// Kind 与四个计数是聚合的骨架：空 Kind 分不出轮次类型，负数 token 会把合计
	// 抹平（看起来比实际花得少）。宁可拒绝这一条，也不让账本带上无法解释的数字。
	e.Kind = strings.TrimSpace(e.Kind)
	if e.Kind == "" {
		return fmt.Errorf("usage: empty kind")
	}
	if e.Prompt < 0 || e.Completion < 0 || e.Cached < 0 || e.CacheWrite < 0 {
		return fmt.Errorf("usage: negative token counts")
	}
	// 三个可选的标识来自插件与配置，前后空白只会造成「同一个模型两个分组」。
	e.Origin = strings.TrimSpace(e.Origin)
	e.Provider = strings.TrimSpace(e.Provider)
	e.Model = strings.TrimSpace(e.Model)
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	// 分片日期在这里定死：用户说的「今天」是写入方墙上时间的那一天，用 UTC 切会
	// 让东八区凌晨的记录落到前一个文件里；而读取方可能不在同一时区，按读时的
	// 本地日期重算会把同一条记录数进两天。所以日期随行落盘，调用方给的 Day 不采信。
	e.Day = e.TS.In(time.Local).Format(dayFormat)

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal usage entry: %w", err)
	}
	line = append(line, '\n')

	// 建目录幂等，放在锁外：并发写不该为了一个早就存在的目录排队。
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return fmt.Errorf("create usage dir: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	path := filepath.Join(l.dir, e.Day+".jsonl")
	// 上次写到一半（进程被杀、磁盘写满）会留下没有换行结尾的半条记录。直接拼上去
	// 会让「残行 + 新记录」合成一行，读取端把整行当损坏跳过——新记录也跟着丢了。
	if needsSeparator(path) {
		line = append([]byte{'\n'}, line...)
	}
	f, err := openAppend(path)
	if err != nil {
		return fmt.Errorf("open usage file: %w", err)
	}
	defer f.Close()
	// 一行一次 Write：单进程由 mutex 串行；O_APPEND 保证追加不覆盖，跨进程不保证
	// 单行原子，损坏行由读取端跳过。
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("append usage entry: %w", err)
	}
	return nil
}

// needsSeparator 报告 p 的末尾是否缺一个换行，追加新行之前要先补上。
//
// 只补不截断：那半条可能其实是一条完整记录、只是缺换行，补上反而把它修好了；
// 截掉就等于替用户删掉一条真花过的钱。补一个空行最坏只是多一行噪声（读取端本来就
// 跳过空行），不补则可能连新记录一起丢。
//
// 用独立的只读句柄看最后一个字节，不借写句柄：O_APPEND 的句柄读不到内容，而先写
// 再看就晚了。读不到（含 Windows 上杀毒软件短暂独占）按「需要分隔」处理——宁可多
// 一个空行。文件不存在时不必分隔：那是新分片，没有残行可接。
func needsSeparator(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return true
	}
	if info.Size() == 0 {
		return false
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
		return true
	}
	return last[0] != '\n'
}

// openAppend 打开当天的分片，Windows 上遇瞬时占用会重试。
func openAppend(p string) (*os.File, error) {
	const flags = os.O_APPEND | os.O_CREATE | os.O_WRONLY
	f, err := os.OpenFile(p, flags, 0o600)
	if err == nil || runtime.GOOS != "windows" {
		return f, err
	}
	// Windows 上杀毒软件与索引器会短暂独占刚建出来或刚写过的文件，表现为
	// "Access is denied"。照 atomicfile 的节奏等约 75ms；MkdirAll 与 Write 不重试
	// ——目录与已有的句柄不该再被抢，重试只会把真错误拖慢。
	delay := 5 * time.Millisecond
	for i := 0; i < 4; i++ {
		time.Sleep(delay)
		delay *= 2
		if f, err = os.OpenFile(p, flags, 0o600); err == nil {
			return f, nil
		}
	}
	return nil, err
}

// DayBounds 返回 ts 所在当地日的 [from, to)，是一个**绝对时间区间**。
//
// 注意它只表示该当地日期**第一次出现**的那一段时间。跨午夜回拨的城市（如
// America/Goose_Bay 2010-11-07 00:01 拨回 23:01）会让同一个当地日期在时间轴上
// 出现两段，这个区间盖不住回拨之后的那一段，用它查 Totals 会漏掉那些记录。按
// 自然日统计请用 TotalsOnDay / SummaryOnDay（或 Summary 的 GroupByDay，那按条目
// 落盘的 Day 分组）。MonthBounds 同一限制，只是要跨月回拨才会触发。
//
// 两端都取「当地日期实际开始的那一瞬间」，而不是给某个时刻加 24 小时：夏令时切换
// 那天只有 23 或 25 小时，按固定步长切会让相邻两段错开一小时，日与月的合计对不上。
// 也不是 time.Date(y, m, d, 0, 0, 0, 0, time.Local) 造零点——午夜被跳过的城市
// （圣地亚哥 2026-09-06 00:00 → 01:00）会把它归一化到前一天 23:00，那一天就多算了
// 前一小时，相邻两段还会重叠。整日被跳过的极端情况（萨摩亚 2011-12-30）见 dayStart。
func DayBounds(ts time.Time) (from, to time.Time) {
	t := ts.In(time.Local)
	y, m, d := t.Date()
	return dayStart(y, m, d, time.Local), dayStart(y, m, d+1, time.Local)
}

// MonthBounds 返回 ts 所在当地月的 [from, to)，也是一个**绝对时间区间**，口径与
// DayBounds 相同。
//
// 跨月回拨的城市（America/Goose_Bay 2009-11-01 00:01 -03:00 拨回 2009-10-31
// 23:01 -04:00）会让同一个当地月在时间轴上出现两段，这个区间只盖得住第一段；回拨
// 之后落盘为 10-31 的记录在区间之外，用它查 Totals 会漏掉。按自然月统计请用
// TotalsOnMonth / SummaryOnMonth。
func MonthBounds(ts time.Time) (from, to time.Time) {
	t := ts.In(time.Local)
	y, m, _ := t.Date()
	return dayStart(y, m, 1, time.Local), dayStart(y, m+1, 1, time.Local)
}

// dayStart 返回 loc 里某一天实际开始的瞬间。
//
// 日期整天不存在时（萨摩亚跳过 2011-12-30）按日历往后找最近一个存在的日期，这样
// 相邻窗口仍然首尾相接，跳过的那天窗口退化成空区间（from == to）——空窗口进去是
// 零条记录，正是那天该有的答案。最多找一周：时区规则里最长的跳过量级也就一天。
func dayStart(y int, m time.Month, d int, loc *time.Location) time.Time {
	for i := 0; i < 7; i++ {
		// 日期算术走 UTC 的日历日：用本地时区算会在夏令时那天漂成前一天。
		date := time.Date(y, m, d+i, 0, 0, 0, 0, time.UTC).Format(dayFormat)
		if t, ok := clock.FirstInstantOfDate(date, loc); ok {
			return t
		}
	}
	// 连续七天都判成不存在只可能是时区库坏了。退回 time.Date 的归一化结果：
	// 给一个确定的值，总比让查询在这一刻崩掉好。
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// Totals 返回 [from, to) 区间内的合计。to 早于 from 报错；from == to 合法（零结果）。
func (l *Ledger) Totals(from, to time.Time) (Totals, error) {
	var t Totals
	skipped, err := l.scan(from, to, func(e Entry) { t.add(e) })
	t.SkippedFiles = skipped
	return t, err
}

// TotalsOnDay 返回落盘日期为 day（YYYY-MM-DD）的那一天的合计。
//
// 按「条目自己的 Day」筛，而不是按 DayBounds 的时间区间：跨午夜回拨的城市里同一个
// 当地日期会分成两段，区间只能表示第一次出现的那一段（见 DayBounds）。没有 day
// 字段的旧行按分片名补 Day（见 scanFile），所以换时区读也还是同一天。day 非法时报
// 错——它会被拼成文件名，不能让 "../x" 这类值落到目录外面。那天的分片不存在时返回
// 零值与 nil：第一次写入之前就是这样。
func (l *Ledger) TotalsOnDay(day string) (Totals, error) {
	var t Totals
	skipped, err := l.scanDay(day, func(e Entry) { t.add(e) })
	t.SkippedFiles = skipped
	return t, err
}

// Summary 返回 [from, to) 区间内的合计与按 groupBy 拆分的分组。
// groupBy 取 GroupByDay / GroupBySource / GroupByModel / GroupByKind /
// GroupByProvider / GroupByProviderModel。
func (l *Ledger) Summary(from, to time.Time, groupBy string) (Summary, error) {
	s, err := newSummarizer(groupBy)
	if err != nil {
		return Summary{}, err
	}
	skipped, err := l.scan(from, to, s.add)
	if err != nil {
		return Summary{}, err
	}
	return s.result(skipped), nil
}

// SummaryOnDay 与 Summary 同形，但只算落盘日期为 day 的那一天（筛法见 TotalsOnDay）。
func (l *Ledger) SummaryOnDay(day, groupBy string) (Summary, error) {
	s, err := newSummarizer(groupBy)
	if err != nil {
		return Summary{}, err
	}
	skipped, err := l.scanDay(day, s.add)
	if err != nil {
		return Summary{}, err
	}
	return s.result(skipped), nil
}

// TotalsOnMonth 返回落盘月份为 month（YYYY-MM）的那个自然月的合计。
//
// 按条目的 Day 前缀筛，而不是按 MonthBounds 的时间区间：跨月回拨的城市里同一个
// 当地月会分成两段，区间只能表示第一次出现的那一段（见 MonthBounds）。month 非法
// 时报错——它会参与分片名筛选，不能让 "../x" 这类值落到目录外面。那个月的分片一个
// 都不存在时返回零值与 nil。
func (l *Ledger) TotalsOnMonth(month string) (Totals, error) {
	var t Totals
	skipped, err := l.scanMonth(month, func(e Entry) { t.add(e) })
	t.SkippedFiles = skipped
	return t, err
}

// SummaryOnMonth 与 Summary 同形，但只算落盘月份为 month 的那个自然月（筛法见
// TotalsOnMonth）。
func (l *Ledger) SummaryOnMonth(month, groupBy string) (Summary, error) {
	s, err := newSummarizer(groupBy)
	if err != nil {
		return Summary{}, err
	}
	skipped, err := l.scanMonth(month, s.add)
	if err != nil {
		return Summary{}, err
	}
	return s.result(skipped), nil
}

// summarizer 一边累进合计与分组。区间查询与按日查询共用它，分组口径因此只有一份。
type summarizer struct {
	keyOf  func(Entry) string
	total  Totals
	groups map[string]*Totals
}

func newSummarizer(groupBy string) (*summarizer, error) {
	keyOf, err := groupKeyFunc(groupBy)
	if err != nil {
		return nil, err
	}
	return &summarizer{keyOf: keyOf, groups: map[string]*Totals{}}, nil
}

func (s *summarizer) add(e Entry) {
	s.total.add(e)
	k := s.keyOf(e)
	g, ok := s.groups[k]
	if !ok {
		g = &Totals{}
		s.groups[k] = g
	}
	g.add(e)
}

// result 收口成分组按键字典序排列的 Summary。skipped 只填在合计上。
func (s *summarizer) result(skipped int) Summary {
	s.total.SkippedFiles = skipped
	out := Summary{Total: s.total, Groups: make([]GroupTotals, 0, len(s.groups))}
	for k, g := range s.groups {
		out.Groups = append(out.Groups, GroupTotals{Key: k, Totals: *g})
	}
	sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].Key < out.Groups[j].Key })
	return out
}

// groupKeyFunc 给出 groupBy 对应的分组键取法。未知维度报错，不再悄悄按某个默认维度算。
func groupKeyFunc(groupBy string) (func(Entry) string, error) {
	switch groupBy {
	case GroupByDay:
		// 用记录落盘时的日期，与分片口径一致：同一条记录无论谁来读都属于同一天。
		// 扫描时已用分片文件名补齐了没有 day 字段的旧行，这里的本地时区兜底只是
		// 防御——真走到那里说明有人绕过了 scanFile。
		return func(e Entry) string {
			if e.Day != "" {
				return e.Day
			}
			return e.TS.In(time.Local).Format(dayFormat)
		}, nil
	case GroupBySource:
		return func(e Entry) string {
			if k := strings.TrimSpace(e.Origin); k != "" {
				return k
			}
			return foregroundKey
		}, nil
	case GroupByModel:
		return func(e Entry) string { return keyOrUnknown(e.Model) }, nil
	case GroupByKind:
		return func(e Entry) string { return keyOrUnknown(e.Kind) }, nil
	case GroupByProvider:
		// 「这个月是哪家花的钱」：模型名会随提供商变，提供商才是账单上的那一方。
		return func(e Entry) string { return keyOrUnknown(e.Provider) }, nil
	case GroupByProviderModel:
		// 两段都为空时落到 unknown / unknown：价格挂在模型上，这个键正好是查价用的
		// 那一对，界面上按它列的金额能与价格表一行行对上。
		return func(e Entry) string {
			return keyOrUnknown(e.Provider) + " / " + keyOrUnknown(e.Model)
		}, nil
	}
	return nil, fmt.Errorf("usage: unknown group by %q", groupBy)
}

// keyOrUnknown 把空键（含只有空白）归到一个可读的键，避免界面上出现一行没有标题
// 的数字。
func keyOrUnknown(s string) string {
	if k := strings.TrimSpace(s); k != "" {
		return k
	}
	return unknownKey
}

func (t *Totals) add(e Entry) {
	t.Requests++
	t.Prompt += e.Prompt
	t.Completion += e.Completion
	t.Cached += e.Cached
	t.CacheWrite += e.CacheWrite
}

// scan 逐条读出 [from, to) 区间内的记录，返回整份跳过（读不出来）的分片数。
//
// 目录不存在视为空账本（第一次写入之前就是这样），不算错误；目录本身读不了仍然
// 报错——那是配置问题，不是账本暂时没内容。单个分片读不出来只跳过并计数，与
// session.Store.Get 的口径一致：坏了一个文件不该让整份账本查不出来，钱已经花掉了。
func (l *Ledger) scan(from, to time.Time, visit func(Entry)) (skipped int, err error) {
	if to.Before(from) {
		return 0, fmt.Errorf("usage: range end %s is before start %s",
			to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	// 分片名预筛：日期串定长且高位在前，字典序即时间序，可以直接比字符串。区间是
	// 闭的——余量已经把边界放宽到 ±48 小时，宁可多开一个文件，不可漏一条。
	lowDay := from.In(time.Local).Add(-scanDaySlack).Format(dayFormat)
	highDay := to.In(time.Local).Add(scanDaySlack).Format(dayFormat)
	for _, de := range entries {
		if de.IsDir() || !dayFilePattern.MatchString(de.Name()) {
			continue
		}
		if day := de.Name()[:len(dayFormat)]; day < lowDay || day > highDay {
			continue
		}
		if !scanFile(filepath.Join(l.dir, de.Name()), strings.TrimSuffix(de.Name(), ".jsonl"), inRange(from, to), visit) {
			skipped++
		}
	}
	return skipped, nil
}

// inRange 是区间查询的谓词：半开的 [from, to)。from 当刻计入，to 当刻不计入——
// 相邻两段首尾相接时不重不漏。
func inRange(from, to time.Time) func(Entry) bool {
	return func(e Entry) bool {
		return !e.TS.Before(from) && e.TS.Before(to)
	}
}

// scanDay 只读 day 对应的那一个分片，返回跳过数（0 或 1）。
//
// 分片名由 day 拼成，所以先校验它是合法的 YYYY-MM-DD：非法值（"../x"、空串、
// "2010-13-01"）直接报错，绝不落到目录外面去。分片不存在时不算跳过——那只是那天
// 还没有记录。
func (l *Ledger) scanDay(day string, visit func(Entry)) (skipped int, err error) {
	if err := validateDay(day); err != nil {
		return 0, err
	}
	// 按落盘的 Day 筛，而不是按时间区间：跨午夜回拨会把同一个当地日期分成两段，
	// 区间只能表示第一次出现的那一段。没有 day 字段的旧行由 scanFile 用分片名补上，
	// 所以这里只比字符串；口径与写入时一致，也与区间查询的 GroupByDay 一致。
	keep := func(e Entry) bool { return e.Day == day }
	if !scanFile(filepath.Join(l.dir, day+".jsonl"), day, keep, visit) {
		skipped++
	}
	return skipped, nil
}

// scanMonth 只读 month（YYYY-MM）对应的那些分片，返回跳过数。
//
// 与 scanDay 同理，month 先校验成严格合法的 YYYY-MM 再参与筛选——它会被拼进名字
// 前缀，"../x" 之类不能落到目录外面去。筛选按条目的 Day 前缀而不是分片名：分片名
// 只负责少开文件（dayFilePattern 保证形状），归属仍以条目落盘的 Day 为准。整个月
// 一个分片都没有时返回零值、nil。
func (l *Ledger) scanMonth(month string, visit func(Entry)) (skipped int, err error) {
	if err := validateMonth(month); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	prefix := month + "-"
	keep := func(e Entry) bool { return strings.HasPrefix(e.Day, prefix) }
	for _, de := range entries {
		name := de.Name()
		// dayFilePattern 挡住备份、临时文件与目录；前缀筛掉别的月份。
		if de.IsDir() || !dayFilePattern.MatchString(name) || !strings.HasPrefix(name, prefix) {
			continue
		}
		if !scanFile(filepath.Join(l.dir, name), strings.TrimSuffix(name, ".jsonl"), keep, visit) {
			skipped++
		}
	}
	return skipped, nil
}

// validateDay 确认 day 是可以直接当分片名用的当地日期。
//
// 除了解析成功，还要求格式化回来与原文逐字节一致：Parse 对某些写法比较宽容，
// 而文件名与磁盘上的分片名必须严格对齐。
func validateDay(day string) error {
	t, err := time.Parse(dayFormat, day)
	if err != nil {
		return fmt.Errorf("usage: invalid day %q: %w", day, err)
	}
	if t.Format(dayFormat) != day {
		return fmt.Errorf("usage: invalid day %q", day)
	}
	return nil
}

// validateMonth 确认 month 是可以直接当分片名前缀用的自然月。校验方式与 validateDay
// 相同：解析成功还不够，格式化回来必须与原文逐字节一致（Parse 对 "2009-1" 这类写法
// 比较宽容，而它不能当成 2009 年 1 月用）。
func validateMonth(month string) error {
	t, err := time.Parse(monthFormat, month)
	if err != nil {
		return fmt.Errorf("usage: invalid month %q: %w", month, err)
	}
	if t.Format(monthFormat) != month {
		return fmt.Errorf("usage: invalid month %q", month)
	}
	return nil
}

// scanFile 读一个分片，只把 keep 认下的条目交给 visit，返回它是否完整读完了。
// 打不开（除已被删掉）或中途读错都返回 false，由调用方记成一次跳过；已经读出的
// 条目照样计入。
//
// fileDay 是这个分片的名字（YYYY-MM-DD）。没有 day 字段的旧行一律按它补上：落盘时
// 分片名就是按写入方的当地日期算出来的，它才是这条记录真正的归属；用读时的本机
// 时区重算，换一个时区就把同一条记录数进另一天。补在交给 keep/visit 之前，单日
// 查询、区间分组与按日筛选因此共用同一个口径。
func scanFile(p, fileDay string, keep func(Entry) bool, visit func(Entry)) bool {
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true // 列目录与打开之间被删掉了，不算坏文件
		}
		return false
	}
	defer f.Close()

	// 起始缓冲不超过上限：给了比上限还大的初始容量时，scanner 会先把整行读进来，
	// 上限就成了摆设（测试里把上限调小，就是靠这一点立刻判超长）。
	initial := 64 * 1024
	if maxLineBytes < initial {
		initial = maxLineBytes
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, initial), maxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // 跳过损坏行
		}
		// `null`、`{}` 与缺 ts 的行都能解成零值 Entry 且不报错，但它们不是真实请求。
		// 不在这里拦住，补上 Day 之后按日/按月查询会把它们各算一次，而区间查询又因为
		// 没有有效 TS 把它们丢掉，同一段日子两种口径的合计就对不上。Append 自己会把
		// 零值 TS 改成当前时刻，所以合法记录不会走到这一支。
		if e.TS.IsZero() {
			continue
		}
		if e.Day == "" {
			e.Day = fileDay
		}
		// 区间是半开的 [from, to)：from 当刻计入，to 当刻不计入——相邻两段
		// 首尾相接时不重不漏。
		if !keep(e) {
			continue
		}
		visit(e)
	}
	return sc.Err() == nil
}
