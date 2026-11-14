package agenda

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"wen/internal/plugin"
)

func stateTestPlugin(t *testing.T, now time.Time) *Plugin {
	t.Helper()
	p := New(nil)
	p.lang = "zh"
	p.dayStartHour = defDayStartHour
	p.base = filepath.Join(t.TempDir(), "agenda")
	p.now = func() time.Time { return now }
	return p
}

func TestStateEmpty(t *testing.T) {
	p := stateTestPlugin(t, time.Date(2026, 11, 15, 10, 0, 0, 0, time.Local))
	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("空日程的 State = %+v，期望一块带 Empty 的空块", secs)
	}
}

func TestStateDayRows(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	day := p.snapshot().today(now)
	items := []Item{
		{ID: "a2", Title: "改方案", Start: "14:00", End: "16:00", Status: statusOngoing, Place: "书房"},
		{ID: "a1", Title: "整理提案", Start: "09:30", End: "10:30", Status: statusDone},
		{ID: "a3", Title: "做晚饭", Start: "19:00", End: "20:00", Status: statusPlanned},
		{ID: "a4", Title: "取消的事", Start: "12:00", End: "12:30", Status: statusCancelled},
	}
	if err := p.storeFor("").SavePlan(Plan{Date: day.Format(dateLayout), PlannedAt: now, Items: items}); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 {
		t.Fatalf("State = %+v，期望一块", secs)
	}
	got := secs[0]
	if got.Summary != "共 3 项 · 进行中 1 项" {
		t.Errorf("summary = %q", got.Summary)
	}
	if len(got.Rows) != 3 {
		t.Fatalf("rows = %+v", got.Rows)
	}
	// 按开始时间排序，取消的不出现
	if got.Rows[0].Value != "整理提案" || got.Rows[1].Value != "改方案" || got.Rows[2].Value != "做晚饭" {
		t.Errorf("rows 顺序不对: %+v", got.Rows)
	}
	if got.Rows[0].Tone != plugin.StateToneMuted {
		t.Errorf("已完成项应为 muted: %+v", got.Rows[0])
	}
	if got.Rows[1].Detail != "进行中 · 书房" {
		t.Errorf("进行中项的 detail = %q", got.Rows[1].Detail)
	}
}

func TestStateCommitments(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	if _, err := p.storeFor("").UpdateCommitments(func(cs *[]Commitment, _ func() string) (bool, error) {
		*cs = append(*cs,
			Commitment{ID: "c2", Date: "2026-11-20", Start: "15:00", Title: "见面", Place: "书店"},
			Commitment{ID: "c1", Date: "2026-11-16", Start: "09:00", Title: "面试"},
			Commitment{ID: "c0", Date: "2026-11-01", Start: "09:00", Title: "过去的事"},
		)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || secs[0].Key != "commitments" {
		t.Fatalf("State = %+v，期望只有约定一块", secs)
	}
	got := secs[0]
	if got.Summary != "2 条" || len(got.Rows) != 2 {
		t.Fatalf("summary=%q rows=%+v", got.Summary, got.Rows)
	}
	if got.Rows[0].Value != "面试" || got.Rows[1].Value != "见面" {
		t.Errorf("约定顺序不对: %+v", got.Rows)
	}
}

// 表读坏了不能退化成「今天还没有排表」：那会让人以为今天真的没有安排。
func TestStateReadFailureIsNotAnEmptyState(t *testing.T) {
	p := stateTestPlugin(t, time.Date(2026, 11, 15, 10, 0, 0, 0, time.Local))
	if err := os.MkdirAll(p.base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.base, "plan.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if secs := p.State(plugin.StateQuery{Lang: "zh"}); len(secs) != 0 {
		t.Fatalf("读失败时 State = %+v，期望不报空状态", secs)
	}
}

// day_start_hour 之前的 01:00 属于次日凌晨，必须排在 23:00 之后。
func TestStateDaySortsAcrossLogicalMidnight(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	p.dayStartHour = 5
	day := p.snapshot().today(now)
	items := []Item{
		{ID: "c", Title: "次日凌晨", Start: "01:00", End: "02:00", Status: statusPlanned},
		{ID: "a", Title: "上午", Start: "09:00", End: "10:00", Status: statusPlanned},
		{ID: "b", Title: "深夜", Start: "23:00", End: "23:30", Status: statusPlanned},
	}
	if err := p.storeFor("").SavePlan(Plan{Date: day.Format(dateLayout), Items: items}); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || len(secs[0].Rows) != 3 {
		t.Fatalf("State = %+v", secs)
	}
	var got []string
	for _, r := range secs[0].Rows {
		got = append(got, r.Value)
	}
	want := []string{"上午", "深夜", "次日凌晨"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("排序 = %v，期望 %v", got, want)
		}
	}
}
