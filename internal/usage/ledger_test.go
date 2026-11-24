package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata" // 本机可能没有 IANA 时区库（Windows 常见），夏令时用例靠它兜底
)

func mustOpen(t *testing.T, dir string) *Ledger {
	t.Helper()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	return l
}

func mustAppend(t *testing.T, l *Ledger, e Entry) {
	t.Helper()
	if err := l.Append(e); err != nil {
		t.Fatalf("Append(%+v): %v", e, err)
	}
}

func checkPerm(t *testing.T, p string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // Windows 上没有 POSIX 权限位
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat(%q): %v", p, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s: perm = %o, want %o", p, got, want)
	}
}

// setLocal 把 time.Local 换成 loc，测试结束还原。测试都不并行，换全局变量安全。
func setLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

// lineJSON 把一条记录编成落盘的一行，用于绕开 Append 的校验造旧记录。
func lineJSON(t *testing.T, e Entry) string {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal %+v: %v", e, err)
	}
	return string(raw)
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("空目录应当报错")
	}
}

func TestAppendWritesOneLinePerDay(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "usage")
	l := mustOpen(t, dir)

	ts := time.Date(2026, 3, 4, 10, 30, 0, 0, time.Local)
	want := Entry{TS: ts, Kind: KindTurn, Model: "m1", Prompt: 11, Completion: 7, Cached: 3, CacheWrite: 2}
	mustAppend(t, l, want)

	path := filepath.Join(dir, "2026-03-04.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(raw)
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("line must end with \\n, got %q", text)
	}
	if strings.Count(text, "\n") != 1 {
		t.Fatalf("one Append must write exactly one line, got %q", text)
	}
	var back Entry
	if err := json.Unmarshal([]byte(strings.TrimSuffix(text, "\n")), &back); err != nil {
		t.Fatalf("written line is not JSON: %v", err)
	}
	if !back.TS.Equal(want.TS) || back.Kind != want.Kind || back.Model != want.Model ||
		back.Prompt != want.Prompt || back.Completion != want.Completion ||
		back.Cached != want.Cached || back.CacheWrite != want.CacheWrite {
		t.Fatalf("round trip = %+v, want %+v", back, want)
	}
	if back.Day != ts.Format(dayFormat) {
		t.Fatalf("Day = %q, want %q（分片日期要随行落盘）", back.Day, ts.Format(dayFormat))
	}

	checkPerm(t, path, 0o600)
	checkPerm(t, dir, 0o700)

	// 归零的 TS 用当前时刻，落到今天的文件里。
	before := time.Now().In(time.Local).Format(dayFormat)
	mustAppend(t, l, Entry{Kind: KindTurn, Prompt: 1})
	after := time.Now().In(time.Local).Format(dayFormat)
	gotToday := false
	for _, day := range []string{before, after} {
		if _, err := os.Stat(filepath.Join(dir, day+".jsonl")); err == nil {
			gotToday = true
		}
	}
	if !gotToday {
		t.Fatalf("zero TS must fall back to now, no file for %s/%s", before, after)
	}
}

func TestAppendSplitsAcrossLocalMidnight(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)

	late := time.Date(2026, 3, 4, 23, 59, 0, 0, time.Local)
	early := late.Add(2 * time.Minute) // 次日 00:01
	mustAppend(t, l, Entry{TS: late, Kind: KindTurn, Prompt: 1})
	mustAppend(t, l, Entry{TS: early, Kind: KindTurn, Prompt: 2})

	for _, day := range []string{"2026-03-04", "2026-03-05"} {
		p := filepath.Join(dir, day+".jsonl")
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s 应当存在: %v", day, err)
		}
		if strings.Count(string(raw), "\n") != 1 {
			t.Fatalf("%s 应当只有一条记录，实际 %q", day, string(raw))
		}
	}
	from, _ := DayBounds(late)
	_, to := DayBounds(early)
	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if tot.Requests != 2 || tot.Prompt != 3 {
		t.Fatalf("Totals = %+v, want 2 requests / prompt 3", tot)
	}
}

// 进程被杀或磁盘写满会留下半条不带换行的记录。下一行直接拼上去，读取端会把
// 「残行 + 新记录」当成一行损坏跳过——新记录也跟着丢。追加前要先补一个换行。
func TestAppendSeparatesUnterminatedTail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)
	ts := time.Date(2026, 11, 23, 12, 0, 0, 0, time.Local)
	dayFile := filepath.Join(dir, ts.Format(dayFormat))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 半条记录：写到一半就断了，末尾没有换行
	full := lineJSON(t, Entry{TS: ts, Kind: KindTurn, Prompt: 11})
	if err := os.WriteFile(dayFile, []byte(full[:len(full)/2]), 0o600); err != nil {
		t.Fatal(err)
	}

	mustAppend(t, l, Entry{TS: ts.Add(time.Minute), Kind: KindTurn, Prompt: 73})

	from, to := DayBounds(ts)
	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	// 残行本身读不出来（照旧跳过），但新记录必须读得出来：一条请求、73 个 prompt
	if tot.Requests != 1 || tot.Prompt != 73 {
		t.Fatalf("Totals = %+v, want 1 request / prompt 73", tot)
	}
}

func TestTotalsHalfOpenRange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)

	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.Local)
	_, to := DayBounds(from)
	for _, e := range []Entry{
		{TS: from.Add(-time.Second), Prompt: 1}, // 区间之前
		{TS: from, Prompt: 2},                   // from 当刻，计入
		{TS: from.Add(time.Hour), Prompt: 4},    // 区间内
		{TS: to.Add(-time.Second), Prompt: 8},   // 区间内最后一刻
		{TS: to, Prompt: 16},                    // to 当刻，不计入
		{TS: to.Add(time.Second), Prompt: 32},   // 区间之后
	} {
		e.Kind = KindTurn
		e.Completion = 1
		mustAppend(t, l, e)
	}

	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	want := Totals{Requests: 3, Prompt: 14, Completion: 3}
	if tot != want {
		t.Fatalf("Totals = %+v, want %+v", tot, want)
	}

	// 紧邻的下一段只拿到 to 当刻及之后的那几条。
	nextFrom, nextTo := DayBounds(to)
	next, err := l.Totals(nextFrom, nextTo)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if next.Requests != 2 || next.Prompt != 48 {
		t.Fatalf("next window = %+v, want 2 requests / prompt 48", next)
	}
}

// 分片名预筛：区间外的文件根本不打开，区间内的条目一条不漏。
func TestTotalsReadsOnlyShardRange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)

	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.Local)
	const shards = 400
	for i := 0; i < shards; i++ {
		mustAppend(t, l, Entry{TS: base.AddDate(0, 0, i), Kind: KindTurn, Prompt: i + 1})
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != shards {
		t.Fatalf("分片数 = %d, want %d", len(files), shards)
	}

	first, last := 198, 199
	from, _ := DayBounds(base.AddDate(0, 0, first))
	_, to := DayBounds(base.AddDate(0, 0, last))
	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	want := Totals{Requests: 2, Prompt: (first + 1) + (last + 1)}
	if tot != want {
		t.Fatalf("Totals = %+v, want %+v（区间外分片不该混进来）", tot, want)
	}

	// 半开区间：只取最后一天，就只剩最后一天那条。
	lastFrom, lastTo := DayBounds(base.AddDate(0, 0, last))
	one, err := l.Totals(lastFrom, lastTo)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if one != (Totals{Requests: 1, Prompt: last + 1}) {
		t.Fatalf("单日 Totals = %+v, want 1 requests / prompt %d", one, last+1)
	}
}

func TestRangeValidation(t *testing.T) {
	l := mustOpen(t, filepath.Join(t.TempDir(), "usage"))
	at := time.Date(2026, 4, 2, 0, 0, 0, 0, time.Local)

	if _, err := l.Totals(at, at.Add(-time.Second)); err == nil {
		t.Fatal("to 早于 from 应当报错")
	}
	if _, err := l.Summary(at, at.Add(-time.Hour), GroupByDay); err == nil {
		t.Fatal("Summary 也应当报错")
	}

	// from == to 合法：空区间就是零结果。
	tot, err := l.Totals(at, at)
	if err != nil {
		t.Fatalf("空区间不该报错: %v", err)
	}
	if tot != (Totals{}) {
		t.Fatalf("空区间应当零结果，got %+v", tot)
	}
	s, err := l.Summary(at, at, GroupByDay)
	if err != nil {
		t.Fatalf("空区间 Summary 不该报错: %v", err)
	}
	if s.Total != (Totals{}) || len(s.Groups) != 0 {
		t.Fatalf("空区间 Summary = %+v, want 空", s)
	}
}

func TestAggregateSkipsCorruptLinesAndForeignFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := strings.Join([]string{
		`{"ts":"2026-05-01T10:00:00Z","kind":"turn","prompt":10,"completion":2}`,
		`not json at all`,
		``,
		`   `,
		`{"ts":"2026-05-01T11:00:00Z","kind":"compact","prompt":4,"completion":1}`,
		`{"ts":"2026-05-01T12:00:00Z","kind":"tur`, // 被截断的一行
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-05-01.jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	// 名字不匹配的文件一律不读：备份、别的格式都不该被算进账。
	bogus := `{"ts":"2026-05-01T10:00:00Z","kind":"turn","prompt":1000}`
	for _, name := range []string{"notes.jsonl", "2026-05-01.jsonl.tmp", "2026-05-01.json", "2026-5-1.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(bogus+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 名字像分片、其实是目录的也不算。
	sub := filepath.Join(dir, "2026-05-02.jsonl")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "2026-05-02.jsonl"), []byte(bogus+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := mustOpen(t, dir)
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	from, _ := DayBounds(ts)
	_, to := DayBounds(ts.AddDate(0, 0, 1))
	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("损坏行不该让聚合失败: %v", err)
	}
	want := Totals{Requests: 2, Prompt: 14, Completion: 3}
	if tot != want {
		t.Fatalf("Totals = %+v, want %+v", tot, want)
	}
}

// `null` 与 `{}` 都能被 json.Unmarshal 解成零值 Entry 且不报错。补上分片日期之后，
// 按日/按月查询会把它们各算一次请求，而区间查询因为没有有效 TS 又把它们丢掉——
// 同一天两种口径的合计就对不上。零值 TS 的行根本不是请求，扫描时就要跳过。
func TestScanSkipsZeroTimestampRows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)
	ts := time.Date(2026, 5, 2, 10, 0, 0, 0, time.Local)
	mustAppend(t, l, Entry{TS: ts, Kind: KindTurn, Prompt: 11, Completion: 7})

	// 手工追加两行零值记录：它们解出来 TS 是零值，不该被当成请求。
	shard := filepath.Join(dir, "2026-05-02.jsonl")
	f, err := os.OpenFile(shard, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("null\n{}\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	day, err := l.TotalsOnDay("2026-05-02")
	if err != nil {
		t.Fatalf("TotalsOnDay: %v", err)
	}
	if day.Requests != 1 {
		t.Fatalf("TotalsOnDay = %+v, want 1 request（null 与 {} 不是请求）", day)
	}
	// Prompt 合计只来自那条合法记录，与跳过的行无关。
	if day.Prompt != 11 || day.Completion != 7 {
		t.Fatalf("TotalsOnDay = %+v, want prompt 11 / completion 7", day)
	}

	month, err := l.TotalsOnMonth("2026-05")
	if err != nil {
		t.Fatalf("TotalsOnMonth: %v", err)
	}
	if month.Requests != 1 || month.Prompt != 11 || month.Completion != 7 {
		t.Fatalf("TotalsOnMonth = %+v, want 1 request / prompt 11 / completion 7", month)
	}

	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.Local)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local)
	rng, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if rng.Requests != 1 || rng.Prompt != 11 || rng.Completion != 7 {
		t.Fatalf("Totals(2026-05-01..2026-06-01) = %+v, want 1 request / prompt 11 / completion 7", rng)
	}
}

// 一个分片打不开只跳过它，别的分片照算，跳过数在合计里报出来。
func TestScanSkipsUnreadableShard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上打不开的文件是占用，不靠权限位模拟")
	}
	dir := filepath.Join(t.TempDir(), "usage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.Local)
	good := filepath.Join(dir, ts.Format(dayFormat)+".jsonl")
	if err := os.WriteFile(good, []byte(lineJSON(t, Entry{TS: ts, Kind: KindTurn, Prompt: 5})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badIn := filepath.Join(dir, ts.AddDate(0, 0, 1).Format(dayFormat)+".jsonl")
	badOut := filepath.Join(dir, "2020-01-01.jsonl") // 区间之外：连打开都不该
	for _, p := range []string{badIn, badOut} {
		if err := os.WriteFile(p, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o000); err != nil {
			t.Skipf("设不了权限位: %v", err)
		}
		p := p
		t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	}
	if f, err := os.Open(badIn); err == nil {
		f.Close()
		t.Skip("当前身份仍能读 0o000 文件（多半是 root），模拟不了打开失败")
	}

	l := mustOpen(t, dir)
	from, _ := DayBounds(ts)
	_, to := DayBounds(ts.AddDate(0, 0, 1))
	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("单个分片读不出来不该让整次查询失败: %v", err)
	}
	if tot.Requests != 1 || tot.Prompt != 5 {
		t.Fatalf("Totals = %+v, want 1 requests / prompt 5（好文件仍要算）", tot)
	}
	if tot.SkippedFiles != 1 {
		t.Fatalf("SkippedFiles = %d, want 1（区间外的坏文件不该被打开）", tot.SkippedFiles)
	}

	s, err := l.Summary(from, to, GroupByKind)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total.SkippedFiles != 1 {
		t.Fatalf("Summary 合计 SkippedFiles = %d, want 1", s.Total.SkippedFiles)
	}
	for _, g := range s.Groups {
		if g.Totals.SkippedFiles != 0 {
			t.Fatalf("分组里不该填 SkippedFiles: %+v", g)
		}
	}
}

// 超长行让 scanner 报错，这个文件按坏文件跳过；已经读出的行照算，不返回错误。
func TestScanSkipsOverlongLine(t *testing.T) {
	old := maxLineBytes
	maxLineBytes = 512
	t.Cleanup(func() { maxLineBytes = old })

	dir := filepath.Join(t.TempDir(), "usage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 8, 1, 10, 0, 0, 0, time.Local)
	short := lineJSON(t, Entry{TS: ts, Kind: KindTurn, Prompt: 5})
	huge := lineJSON(t, Entry{TS: ts.Add(time.Minute), Kind: KindTurn, Prompt: 9999,
		Model: strings.Repeat("x", 4*1024)})
	if err := os.WriteFile(filepath.Join(dir, ts.Format(dayFormat)+".jsonl"),
		[]byte(short+"\n"+huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	next := ts.AddDate(0, 0, 1)
	if err := os.WriteFile(filepath.Join(dir, next.Format(dayFormat)+".jsonl"),
		[]byte(lineJSON(t, Entry{TS: next, Kind: KindTurn, Prompt: 7})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 区间之外同样有超长行：这个文件连打开都不该，跳过数不能变成 2。
	if err := os.WriteFile(filepath.Join(dir, "2026-01-01.jsonl"), []byte(huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := mustOpen(t, dir)
	from, _ := DayBounds(ts)
	_, to := DayBounds(next)
	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("超长行不该让查询失败: %v", err)
	}
	// 超长行之前的那条照算，超长行本身不算。
	want := Totals{Requests: 2, Prompt: 12, SkippedFiles: 1}
	if tot != want {
		t.Fatalf("Totals = %+v, want %+v", tot, want)
	}
}

func TestSummaryGrouping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)

	day1 := time.Date(2026, 5, 1, 9, 0, 0, 0, time.Local)
	_, day2From := DayBounds(day1)
	day2 := day2From.Add(9 * time.Hour)
	entries := []Entry{
		{TS: day1, Kind: KindTurn, Origin: "", Provider: "p1", Model: "mA", Prompt: 10, Completion: 1},
		{TS: day1, Kind: KindCompact, Origin: "scheduler", Provider: "p1", Model: "mA", Prompt: 20, Completion: 2},
		{TS: day2, Kind: KindTurn, Origin: "agenda", Provider: "p2", Model: "mB", Prompt: 40, Completion: 4},
	}
	for _, e := range entries {
		mustAppend(t, l, e)
	}
	from := day1.Add(-time.Hour)
	to := day2.Add(time.Hour)

	cases := []struct {
		groupBy string
		keys    []string
		totals  []Totals
	}{
		{
			groupBy: GroupByDay,
			keys:    []string{day1.Format(dayFormat), day2.Format(dayFormat)},
			totals: []Totals{
				{Requests: 2, Prompt: 30, Completion: 3},
				{Requests: 1, Prompt: 40, Completion: 4},
			},
		},
		{
			groupBy: GroupBySource,
			// 空 origin 归 foreground，键按字典序。
			keys:   []string{"agenda", "foreground", "scheduler"},
			totals: []Totals{{Requests: 1, Prompt: 40, Completion: 4}, {Requests: 1, Prompt: 10, Completion: 1}, {Requests: 1, Prompt: 20, Completion: 2}},
		},
		{
			groupBy: GroupByModel,
			keys:    []string{"mA", "mB"},
			totals:  []Totals{{Requests: 2, Prompt: 30, Completion: 3}, {Requests: 1, Prompt: 40, Completion: 4}},
		},
		{
			groupBy: GroupByKind,
			keys:    []string{"compact", "turn"},
			totals:  []Totals{{Requests: 1, Prompt: 20, Completion: 2}, {Requests: 2, Prompt: 50, Completion: 5}},
		},
		{
			// 「按提供商看花费」：同一家的不同模型算一组。
			groupBy: GroupByProvider,
			keys:    []string{"p1", "p2"},
			totals:  []Totals{{Requests: 2, Prompt: 30, Completion: 3}, {Requests: 1, Prompt: 40, Completion: 4}},
		},
	}
	for _, c := range cases {
		t.Run(c.groupBy, func(t *testing.T) {
			s, err := l.Summary(from, to, c.groupBy)
			if err != nil {
				t.Fatalf("Summary(%s): %v", c.groupBy, err)
			}
			wantTotal := Totals{Requests: 3, Prompt: 70, Completion: 7}
			if s.Total != wantTotal {
				t.Fatalf("Total = %+v, want %+v", s.Total, wantTotal)
			}
			if !sort.SliceIsSorted(s.Groups, func(i, j int) bool { return s.Groups[i].Key < s.Groups[j].Key }) {
				t.Fatalf("分组没按键排序: %+v", s.Groups)
			}
			if len(s.Groups) != len(c.keys) {
				t.Fatalf("groups = %+v, want keys %v", s.Groups, c.keys)
			}
			for i, k := range c.keys {
				if s.Groups[i].Key != k || s.Groups[i].Totals != c.totals[i] {
					t.Fatalf("group[%d] = %+v, want key %q totals %+v", i, s.Groups[i], k, c.totals[i])
				}
			}
		})
	}

	if _, err := l.Summary(from, to, "week"); err == nil {
		t.Fatal("未知 groupBy 应当报错")
	}
}

// 空键（含只有空白）归到可读的桶：旧记录与外部写入方可能没写 model / kind，
// 这类行也不能让 Append 的校验挡住，所以直接落盘。
func TestEmptyGroupKeysFallBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	lines := []string{
		lineJSON(t, Entry{TS: ts, Prompt: 1}),
		lineJSON(t, Entry{TS: ts.Add(time.Minute), Kind: "   ", Model: "  ", Origin: " \t ", Prompt: 2}),
	}
	shard := filepath.Join(dir, ts.Format(dayFormat)+".jsonl")
	if err := os.WriteFile(shard, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := mustOpen(t, dir)
	from, _ := DayBounds(ts)
	_, to := DayBounds(ts.AddDate(0, 0, 1))
	cases := []struct {
		groupBy string
		want    string
	}{
		{GroupByKind, unknownKey},
		{GroupByModel, unknownKey},
		{GroupByProvider, unknownKey},
		{GroupBySource, foregroundKey},
	}
	for _, c := range cases {
		t.Run(c.groupBy, func(t *testing.T) {
			s, err := l.Summary(from, to, c.groupBy)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Groups) != 1 || s.Groups[0].Key != c.want {
				t.Fatalf("groups = %+v, want 单组 %q", s.Groups, c.want)
			}
			if s.Groups[0].Totals.Requests != 2 {
				t.Fatalf("空键分组应当合并两条，got %+v", s.Groups[0].Totals)
			}
		})
	}
}

func TestEmptyLedgerIsZero(t *testing.T) {
	l := mustOpen(t, filepath.Join(t.TempDir(), "usage"))
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	_, to := DayBounds(from)

	// 目录根本不存在：第一次写入之前就是这样，不算错误。
	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("Totals on missing dir: %v", err)
	}
	if tot != (Totals{}) {
		t.Fatalf("Totals = %+v, want zero", tot)
	}
	s, err := l.Summary(from, to, GroupByDay)
	if err != nil {
		t.Fatalf("Summary on missing dir: %v", err)
	}
	if s.Total != (Totals{}) || len(s.Groups) != 0 {
		t.Fatalf("Summary = %+v, want empty", s)
	}

	// 只读不该留下目录。
	if _, err := os.Stat(l.dir); !os.IsNotExist(err) {
		t.Fatalf("读取不该创建账本目录: %v", err)
	}
}

func TestConcurrentAppendKeepsEveryLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)

	const goroutines, perGoroutine = 50, 4
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local)
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perGoroutine)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				ts := base.Add(time.Duration(i*perGoroutine+j) * time.Second)
				if err := l.Append(Entry{TS: ts, Kind: KindTurn, Prompt: 1, Completion: 2}); err != nil {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Append: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "2026-06-01.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("行数 = %d, want %d（有行被覆盖或被截断）", len(lines), goroutines*perGoroutine)
	}
	for _, line := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("并发写入把行写坏了: %q: %v", line, err)
		}
	}

	from, _ := DayBounds(base)
	_, to := DayBounds(base)
	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := Totals{Requests: goroutines * perGoroutine, Prompt: goroutines * perGoroutine, Completion: 2 * goroutines * perGoroutine}
	if tot != want {
		t.Fatalf("Totals = %+v, want %+v", tot, want)
	}
}

// 分片日期随行落盘：写入方与读取方不在同一时区时，分组键与分片名都不能漂移。
func TestDayFixedAtWriteTime(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)

	old := time.Local
	t.Cleanup(func() { time.Local = old })

	writeZone := time.FixedZone("PLUS9", 9*3600)
	readZone := time.FixedZone("MINUS5", -5*3600)
	ts := time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC) // 东九区已是 1 月 2 日 05:00
	writeDay := ts.In(writeZone).Format(dayFormat)
	if writeDay != "2026-01-02" {
		t.Fatalf("用例前提变了：writeDay = %s", writeDay)
	}

	time.Local = writeZone
	// 调用方给的 Day 不采信，一律由 TS 的当地日期覆盖。
	mustAppend(t, l, Entry{TS: ts, Day: "1999-01-01", Kind: KindTurn, Prompt: 1})

	raw, err := os.ReadFile(filepath.Join(dir, writeDay+".jsonl"))
	if err != nil {
		t.Fatalf("分片应落在写入方的日期下: %v", err)
	}
	var back Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &back); err != nil {
		t.Fatal(err)
	}
	if back.Day != writeDay {
		t.Fatalf("落盘的 day = %q, want %q", back.Day, writeDay)
	}
	if _, err := os.Stat(filepath.Join(dir, "1999-01-01.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("调用方给的 Day 不该被采信: %v", err)
	}

	// 换到另一个时区读：分组键仍是写入时的日期。
	time.Local = readZone
	from, to := ts.Add(-24*time.Hour), ts.Add(24*time.Hour)
	s, err := l.Summary(from, to, GroupByDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Groups) != 1 || s.Groups[0].Key != writeDay {
		t.Fatalf("读时区漂进了分组键: %+v, want 单组 %s", s.Groups, writeDay)
	}
	if s.Total.Requests != 1 {
		t.Fatalf("Total.Requests = %d, want 1", s.Total.Requests)
	}
}

func TestDayBoundsAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("加载 America/New_York: %v（本机缺 IANA 库时应由 time/tzdata 兜住）", err)
	}
	setLocal(t, ny)

	cases := []struct {
		label string
		day   time.Time
		want  time.Duration
	}{
		{"春季前移", time.Date(2026, 3, 8, 12, 0, 0, 0, ny), 23 * time.Hour},
		{"秋季回拨", time.Date(2026, 11, 1, 12, 0, 0, 0, ny), 25 * time.Hour},
		{"平常一天", time.Date(2026, 6, 1, 12, 0, 0, 0, ny), 24 * time.Hour},
	}
	for _, c := range cases {
		from, to := DayBounds(c.day)
		if got := to.Sub(from); got != c.want {
			t.Errorf("%s %s: 窗口跨度 = %v, want %v", c.label, c.day.Format(dayFormat), got, c.want)
		}
		if h, m, s := from.Clock(); h != 0 || m != 0 || s != 0 {
			t.Errorf("%s: 起点不是当地零点: %s", c.label, from)
		}
		// 相邻日窗口首尾相接：前一天的后沿就是这一天的前沿，反之亦然。
		if _, prevTo := DayBounds(from.Add(-time.Hour)); !prevTo.Equal(from) {
			t.Errorf("%s: 前一天的末尾 %s 没接上 %s", c.label, prevTo, from)
		}
		if nextFrom, _ := DayBounds(to.Add(time.Hour)); !nextFrom.Equal(to) {
			t.Errorf("%s: 后一天的开头 %s 没接上 %s", c.label, nextFrom, to)
		}
	}
}

// 有的城市在夏令时切换时直接跳过午夜（圣地亚哥 2026-09-06 00:00 → 01:00）。
// 边界必须取那一天实际开始的瞬间：用 time.Date 造零点会归一化到前一天 23:00，
// 那一天因此多算前一小时，还和前一天重叠。
func TestDayBoundsAcrossSkippedMidnight(t *testing.T) {
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatalf("加载 America/Santiago: %v（本机缺 IANA 库时应由 time/tzdata 兜住）", err)
	}
	setLocal(t, santiago)

	from, to := DayBounds(time.Date(2026, 9, 6, 12, 0, 0, 0, santiago))
	if got := from.In(santiago); got.Day() != 6 || got.Hour() != 1 || got.Minute() != 0 {
		t.Fatalf("09-06 的起点 = %s，期望当地 01:00", got)
	}
	if span := to.Sub(from); span != 23*time.Hour {
		t.Fatalf("09-06 窗口跨度 = %v，期望 23 小时", span)
	}
	// 前一天的后沿就是这一天的前沿：相邻窗口首尾相接，日合计才等于整月。
	if _, prevTo := DayBounds(time.Date(2026, 9, 5, 12, 0, 0, 0, santiago)); !prevTo.Equal(from) {
		t.Fatalf("09-05 的终点 %s 没接上 09-06 的起点 %s", prevTo, from)
	}
}

// 同一天气用例在记账侧的对应物：跳过午夜的那个月，逐日窗口仍要刚好铺满整月。
func TestMonthBoundsAcrossSkippedMidnight(t *testing.T) {
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatalf("加载 America/Santiago: %v", err)
	}
	setLocal(t, santiago)

	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)
	const days = 30 // 2026 年 9 月，含一次跳过午夜
	for d := 1; d <= days; d++ {
		mustAppend(t, l, Entry{TS: time.Date(2026, 9, d, 12, 0, 0, 0, santiago), Kind: KindTurn, Prompt: 1})
	}

	from, to := MonthBounds(time.Date(2026, 9, 15, 0, 0, 0, 0, santiago))
	if !from.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, santiago)) {
		t.Fatalf("月初 = %s, want 9 月 1 日零点", from)
	}
	if want := time.Duration(days*24-1) * time.Hour; to.Sub(from) != want {
		t.Fatalf("九月窗口跨度 = %v, want %v（少了一小时夏令时）", to.Sub(from), want)
	}
	month, err := l.Totals(from, to)
	if err != nil {
		t.Fatal(err)
	}
	if month.Requests != days {
		t.Fatalf("整月 Requests = %d, want %d", month.Requests, days)
	}

	sum, seen := 0, 0
	for cur := from; cur.Before(to); {
		dayFrom, dayTo := DayBounds(cur)
		if !dayFrom.Equal(cur) {
			t.Fatalf("日窗口 %s 没接上前一天的末尾 %s", dayFrom, cur)
		}
		dayTot, err := l.Totals(dayFrom, dayTo)
		if err != nil {
			t.Fatal(err)
		}
		sum += dayTot.Requests
		seen++
		cur = dayTo
	}
	if seen != days {
		t.Fatalf("遍历到 %d 天, want %d", seen, days)
	}
	if sum != month.Requests {
		t.Fatalf("各日合计 %d != 整月 %d", sum, month.Requests)
	}
}

func TestMonthBoundsSumsToDays(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("加载 America/New_York: %v", err)
	}
	setLocal(t, ny)

	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)
	const days = 31 // 2026 年 3 月，含一次春季前移
	for d := 1; d <= days; d++ {
		mustAppend(t, l, Entry{TS: time.Date(2026, 3, d, 12, 0, 0, 0, ny), Kind: KindTurn, Prompt: 1})
	}

	from, to := MonthBounds(time.Date(2026, 3, 15, 0, 0, 0, 0, ny))
	if !from.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, ny)) {
		t.Fatalf("月初 = %s, want 3 月 1 日零点", from)
	}
	if want := time.Duration(days*24-1) * time.Hour; to.Sub(from) != want {
		t.Fatalf("三月窗口跨度 = %v, want %v（少了一小时夏令时）", to.Sub(from), want)
	}
	month, err := l.Totals(from, to)
	if err != nil {
		t.Fatal(err)
	}
	if month.Requests != days {
		t.Fatalf("整月 Requests = %d, want %d", month.Requests, days)
	}

	sum, seen := 0, 0
	for cur := from; cur.Before(to); {
		dayFrom, dayTo := DayBounds(cur)
		if !dayFrom.Equal(cur) {
			t.Fatalf("日窗口 %s 没接上前一天的末尾 %s", dayFrom, cur)
		}
		dayTot, err := l.Totals(dayFrom, dayTo)
		if err != nil {
			t.Fatal(err)
		}
		sum += dayTot.Requests
		seen++
		cur = dayTo
	}
	if seen != days {
		t.Fatalf("遍历到 %d 天, want %d", seen, days)
	}
	if sum != month.Requests {
		t.Fatalf("各日合计 %d != 整月 %d", sum, month.Requests)
	}
}

func TestAppendValidatesEntry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)
	ts := time.Date(2026, 2, 2, 12, 0, 0, 0, time.Local)

	bad := []struct {
		name string
		e    Entry
	}{
		{"空 Kind", Entry{TS: ts}},
		{"只有空白的 Kind", Entry{TS: ts, Kind: "   "}},
		{"负 prompt", Entry{TS: ts, Kind: KindTurn, Prompt: -1}},
		{"负 completion", Entry{TS: ts, Kind: KindTurn, Completion: -1}},
		{"负 cached", Entry{TS: ts, Kind: KindTurn, Cached: -1}},
		{"负 cache write", Entry{TS: ts, Kind: KindTurn, CacheWrite: -1}},
	}
	for _, c := range bad {
		if err := l.Append(c.e); err == nil {
			t.Fatalf("%s 应当被拒绝: %+v", c.name, c.e)
		}
	}
	// 一条都没落盘，连目录都不该被建出来。
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("被拒绝的记录不该留下目录: %v", err)
	}
}

// Probe 只试一次写入：目录可用就把目录建出来，且不留下任何文件。
func TestProbeWritableDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)
	if err := l.Probe(); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("Probe 应把目录建出来: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Probe 不该留下文件: %v", entries)
	}
}

// 路径被一个普通文件占住时，Open 仍然成功（它是惰性的），只有 Probe 才报错。
func TestProbeReportsUnwritableDir(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "usage")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(blocked)
	if err != nil {
		t.Fatalf("Open 应保持惰性: %v", err)
	}
	if err := l.Probe(); err == nil {
		t.Fatal("目录被占住时 Probe 应报错")
	}
}

func TestAppendTrimsFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)
	ts := time.Date(2026, 2, 3, 12, 0, 0, 0, time.Local)
	mustAppend(t, l, Entry{TS: ts, Kind: "  turn ", Origin: " scheduler\t", Provider: " p1 ", Model: "\tmA "})

	raw, err := os.ReadFile(filepath.Join(dir, ts.Format(dayFormat)+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var back Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &back); err != nil {
		t.Fatal(err)
	}
	if back.Kind != KindTurn || back.Origin != "scheduler" || back.Provider != "p1" || back.Model != "mA" {
		t.Fatalf("前后空白没去掉: %+v", back)
	}

	s, err := l.Summary(ts.Add(-time.Hour), ts.Add(time.Hour), GroupBySource)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Groups) != 1 || s.Groups[0].Key != "scheduler" {
		t.Fatalf("分组键 = %+v, want 单组 scheduler", s.Groups)
	}
}

// 跨午夜回拨：同一个当地日期在时间轴上出现两段，DayBounds 只盖住第一次出现的那
// 一段，用它查 Totals 会漏掉回拨之后的记录。按落盘 Day 统计的 TotalsOnDay 才是
// 自然日的口径。
func TestTotalsOnDayAcrossMidnightRollback(t *testing.T) {
	gooseBay, err := time.LoadLocation("America/Goose_Bay")
	if err != nil {
		t.Fatalf("加载 America/Goose_Bay: %v（本机缺 IANA 库时应由 time/tzdata 兜住）", err)
	}
	setLocal(t, gooseBay)

	// 2010-11-07 00:01 ADT(-03) 拨回 2010-11-06 23:01 AST(-04)，所以这一刻
	// 在当地是 11-06 的第二个 23 点段。
	ts := time.Date(2010, 11, 7, 3, 30, 0, 0, time.UTC)
	if local := ts.In(gooseBay); local.Format(dayFormat+" 15:04 -0700") != "2010-11-06 23:30 -0400" {
		t.Fatalf("用例前提变了：ts 在当地是 %s", local)
	}

	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)
	mustAppend(t, l, Entry{TS: ts, Kind: KindTurn, Prompt: 10, Completion: 1})

	raw, err := os.ReadFile(filepath.Join(dir, "2010-11-06.jsonl"))
	if err != nil {
		t.Fatalf("应落在 2010-11-06 的分片里: %v", err)
	}
	var back Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &back); err != nil {
		t.Fatal(err)
	}
	if back.Day != "2010-11-06" {
		t.Fatalf("落盘 Day = %q, want 2010-11-06", back.Day)
	}

	// 区间口径的限制：DayBounds 的终点是 11-07 的第一个瞬间（03:00Z），这条记录在
	// 那之后，所以区间查询拿不到它。
	from, to := DayBounds(ts)
	if !to.Equal(time.Date(2010, 11, 7, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("11-07 的第一个瞬间 = %s, want 03:00Z", to)
	}
	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if tot.Requests != 0 {
		t.Fatalf("Totals(DayBounds) = %+v, want 0（这是区间口径的已知限制）", tot)
	}

	// 按落盘 Day 统计：回拨之后的那一段也算进 11-06。
	byDay, err := l.TotalsOnDay("2010-11-06")
	if err != nil {
		t.Fatalf("TotalsOnDay: %v", err)
	}
	if byDay.Requests != 1 || byDay.Prompt != 10 || byDay.Completion != 1 {
		t.Fatalf("TotalsOnDay = %+v, want 1 requests / prompt 10", byDay)
	}

	// 旧行没有 Day 时按分片名补齐：落在 2010-11-06.jsonl 里就算 11-06。
	old := lineJSON(t, Entry{TS: time.Date(2010, 11, 7, 3, 45, 0, 0, time.UTC), Kind: KindTurn, Prompt: 7})
	shard := filepath.Join(dir, "2010-11-06.jsonl")
	if err := os.WriteFile(shard, append(raw, []byte(old+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	byDay, err = l.TotalsOnDay("2010-11-06")
	if err != nil {
		t.Fatalf("TotalsOnDay: %v", err)
	}
	if byDay.Requests != 2 || byDay.Prompt != 17 {
		t.Fatalf("TotalsOnDay = %+v, want 2 requests / prompt 17（无 Day 的旧行按分片名归属）", byDay)
	}
}

func TestTotalsOnDayValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)

	// day 会被拼成分片名：不是合法 YYYY-MM-DD 的值一律报错，含路径穿越。
	for _, day := range []string{"../x", "2010-13-01", "", "2010-11-06 " /* 尾随空白 */, "20101106"} {
		if _, err := l.TotalsOnDay(day); err == nil {
			t.Fatalf("TotalsOnDay(%q) 应当报错", day)
		}
	}

	// 合法但还没有记录的一天：零值、不报错，也不该把目录建出来。
	tot, err := l.TotalsOnDay("2010-11-06")
	if err != nil {
		t.Fatalf("不存在的分片不该报错: %v", err)
	}
	if tot != (Totals{}) {
		t.Fatalf("TotalsOnDay = %+v, want 零值", tot)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("读取不该创建账本目录: %v", err)
	}
}

// 没有 day 字段的旧行按分片名归属，而不是读时的本机时区：东八区凌晨写的记录落在
// 2026-05-02.jsonl，账本搬到 UTC 之后仍然只算 5 月 2 日，不能漂到 5 月 1 日。
func TestOldRowWithoutDayUsesShardDay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"ts":"2026-05-02T00:30:00+08:00","kind":"turn","prompt":10,"completion":1}`
	if err := os.WriteFile(filepath.Join(dir, "2026-05-02.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setLocal(t, time.UTC)

	l := mustOpen(t, dir)
	day, err := l.TotalsOnDay("2026-05-02")
	if err != nil {
		t.Fatalf("TotalsOnDay(2026-05-02): %v", err)
	}
	if day.Requests != 1 || day.Prompt != 10 || day.Completion != 1 {
		t.Fatalf("TotalsOnDay(2026-05-02) = %+v, want 1 requests / prompt 10", day)
	}
	prev, err := l.TotalsOnDay("2026-05-01")
	if err != nil {
		t.Fatalf("TotalsOnDay(2026-05-01): %v", err)
	}
	if prev != (Totals{}) {
		t.Fatalf("TotalsOnDay(2026-05-01) = %+v, want 零值（TS 在 UTC 下是 5-01 16:30）", prev)
	}

	// 区间查询的按日分组也是同一个口径。
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	s, err := l.Summary(from, to, GroupByDay)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(s.Groups) != 1 || s.Groups[0].Key != "2026-05-02" || s.Groups[0].Totals.Requests != 1 {
		t.Fatalf("Summary 分组 = %+v, want 单组 2026-05-02 / 1 request", s.Groups)
	}
	byDay, err := l.SummaryOnDay("2026-05-02", GroupByDay)
	if err != nil {
		t.Fatalf("SummaryOnDay: %v", err)
	}
	if len(byDay.Groups) != 1 || byDay.Groups[0].Key != "2026-05-02" {
		t.Fatalf("SummaryOnDay 分组 = %+v, want 单组 2026-05-02", byDay.Groups)
	}
}

// 跨月回拨：Goose Bay 2009-11-01 00:01 -03:00 拨回 2009-10-31 23:01 -04:00，
// 所以 03:30Z 那一刻在当地还是 10 月 31 日。MonthBounds 只盖住 10 月第一段，这笔
// 记录在区间外；按落盘 Day 统计的 TotalsOnMonth 才是自然月的口径。
func TestMonthTotalsAcrossMonthRollback(t *testing.T) {
	gooseBay, err := time.LoadLocation("America/Goose_Bay")
	if err != nil {
		t.Fatalf("加载 America/Goose_Bay: %v（本机缺 IANA 库时应由 time/tzdata 兜住）", err)
	}
	setLocal(t, gooseBay)

	ts := time.Date(2009, 11, 1, 3, 30, 0, 0, time.UTC)
	if local := ts.In(gooseBay); local.Format(dayFormat+" 15:04 -0700") != "2009-10-31 23:30 -0400" {
		t.Fatalf("用例前提变了：ts 在当地是 %s", local)
	}

	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)
	mustAppend(t, l, Entry{TS: ts, Kind: KindTurn, Prompt: 10, Completion: 1})

	raw, err := os.ReadFile(filepath.Join(dir, "2009-10-31.jsonl"))
	if err != nil {
		t.Fatalf("应落在 2009-10-31 的分片里: %v", err)
	}
	var back Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &back); err != nil {
		t.Fatal(err)
	}
	if back.Day != "2009-10-31" {
		t.Fatalf("落盘 Day = %q, want 2009-10-31", back.Day)
	}

	byDay, err := l.TotalsOnDay("2009-10-31")
	if err != nil {
		t.Fatalf("TotalsOnDay: %v", err)
	}
	if byDay.Requests != 1 || byDay.Prompt != 10 {
		t.Fatalf("TotalsOnDay = %+v, want 1 requests / prompt 10", byDay)
	}
	oct, err := l.TotalsOnMonth("2009-10")
	if err != nil {
		t.Fatalf("TotalsOnMonth(2009-10): %v", err)
	}
	if oct.Requests != 1 || oct.Prompt != 10 {
		t.Fatalf("TotalsOnMonth(2009-10) = %+v, want 1 requests / prompt 10", oct)
	}
	nov, err := l.TotalsOnMonth("2009-11")
	if err != nil {
		t.Fatalf("TotalsOnMonth(2009-11): %v", err)
	}
	if nov != (Totals{}) {
		t.Fatalf("TotalsOnMonth(2009-11) = %+v, want 零值", nov)
	}

	// 区间口径的已知限制：MonthBounds 只表示 10 月第一次出现的那一段，终点是
	// 11-01 的第一个瞬间（03:00Z），这笔记录在它之后。
	from, to := MonthBounds(ts)
	if !to.Equal(time.Date(2009, 11, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("11-01 的第一个瞬间 = %s, want 03:00Z", to)
	}
	tot, err := l.Totals(from, to)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if tot.Requests != 0 {
		t.Fatalf("Totals(MonthBounds) = %+v, want 0（这是区间口径的已知限制）", tot)
	}

	// SummaryOnMonth 与 TotalsOnMonth 同源。
	s, err := l.SummaryOnMonth("2009-10", GroupByDay)
	if err != nil {
		t.Fatalf("SummaryOnMonth: %v", err)
	}
	if s.Total.Requests != 1 || len(s.Groups) != 1 || s.Groups[0].Key != "2009-10-31" {
		t.Fatalf("SummaryOnMonth = %+v, want 单组 2009-10-31 / 1 request", s)
	}
}

func TestTotalsOnMonthValidation(t *testing.T) {
	l := mustOpen(t, filepath.Join(t.TempDir(), "usage"))

	// month 参与分片名筛选：不是严格合法的 YYYY-MM 一律报错，含路径穿越。
	for _, month := range []string{"../x", "2009-13", "2009-1", "2009-10 ", "200910", ""} {
		if _, err := l.TotalsOnMonth(month); err == nil {
			t.Fatalf("TotalsOnMonth(%q) 应当报错", month)
		}
		if _, err := l.SummaryOnMonth(month, GroupByKind); err == nil {
			t.Fatalf("SummaryOnMonth(%q) 应当报错", month)
		}
	}

	// 合法但还没有记录的月份：零值、不报错。
	tot, err := l.TotalsOnMonth("2009-10")
	if err != nil {
		t.Fatalf("不存在的月份不该报错: %v", err)
	}
	if tot != (Totals{}) {
		t.Fatalf("TotalsOnMonth = %+v, want 零值", tot)
	}
}

func TestSummaryOnDayGrouping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)

	day1 := time.Date(2026, 5, 1, 9, 0, 0, 0, time.Local)
	_, day2From := DayBounds(day1)
	day2 := day2From.Add(9 * time.Hour)
	for _, e := range []Entry{
		{TS: day1, Kind: KindTurn, Origin: "", Provider: "p1", Model: "mA", Prompt: 10, Completion: 1},
		{TS: day1, Kind: KindCompact, Origin: "scheduler", Provider: "p1", Model: "mA", Prompt: 20, Completion: 2},
		{TS: day1.Add(time.Hour), Kind: KindTurn, Origin: "agenda", Provider: "p2", Model: "mB", Prompt: 5, Completion: 1},
		{TS: day2, Kind: KindTurn, Origin: "scheduler", Provider: "p2", Model: "mB", Prompt: 99, Completion: 9},
	} {
		mustAppend(t, l, e)
	}

	cases := []struct {
		groupBy string
		keys    []string
		totals  []Totals
	}{
		{
			groupBy: GroupBySource,
			keys:    []string{"agenda", "foreground", "scheduler"},
			totals: []Totals{
				{Requests: 1, Prompt: 5, Completion: 1},
				{Requests: 1, Prompt: 10, Completion: 1},
				{Requests: 1, Prompt: 20, Completion: 2},
			},
		},
		{
			groupBy: GroupByModel,
			keys:    []string{"mA", "mB"},
			totals:  []Totals{{Requests: 2, Prompt: 30, Completion: 3}, {Requests: 1, Prompt: 5, Completion: 1}},
		},
		{
			groupBy: GroupByDay,
			keys:    []string{day1.Format(dayFormat)},
			totals:  []Totals{{Requests: 3, Prompt: 35, Completion: 4}},
		},
	}
	for _, c := range cases {
		t.Run(c.groupBy, func(t *testing.T) {
			s, err := l.SummaryOnDay(day1.Format(dayFormat), c.groupBy)
			if err != nil {
				t.Fatalf("SummaryOnDay(%s): %v", c.groupBy, err)
			}
			if len(s.Groups) != len(c.keys) {
				t.Fatalf("groups = %+v, want keys %v（下一天的记录不该混进来）", s.Groups, c.keys)
			}
			for i, k := range c.keys {
				if s.Groups[i].Key != k || s.Groups[i].Totals != c.totals[i] {
					t.Fatalf("group[%d] = %+v, want key %q totals %+v", i, s.Groups[i], k, c.totals[i])
				}
			}
		})
	}

	if _, err := l.SummaryOnDay(day1.Format(dayFormat), "week"); err == nil {
		t.Fatal("未知 groupBy 应当报错")
	}
}

// 区间外的大部分分片不该被打开：只读两个分片时,开销应当与分片总数基本无关。
func BenchmarkTotalsShardRange(b *testing.B) {
	dir := filepath.Join(b.TempDir(), "usage")
	l, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.Local)
	const shards = 400
	for i := 0; i < shards; i++ {
		if err := l.Append(Entry{TS: base.AddDate(0, 0, i), Kind: KindTurn, Prompt: i + 1}); err != nil {
			b.Fatal(err)
		}
	}
	from, _ := DayBounds(base.AddDate(0, 0, 200))
	_, to := DayBounds(base.AddDate(0, 0, 200))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.Totals(from, to); err != nil {
			b.Fatal(err)
		}
	}
}
