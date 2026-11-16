package scheduler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wen/internal/atomicfile"
	"wen/internal/prompt"
)

// 任务类型。
const (
	typeOnce     = "once"     // 指定时刻执行一次
	typeInterval = "interval" // 每隔 N 分钟
	typeDaily    = "daily"    // 每天定点
)

// Task 是一条定时任务。
type Task struct {
	ID      string `json:"id"`
	Content string `json:"content"` // 到点后作为轮次输入执行的内容
	Type    string `json:"type"`    // once / interval / daily

	At          time.Time `json:"at,omitempty"`           // once：执行时刻
	IntervalMin int       `json:"interval_min,omitempty"` // interval：周期（分钟）
	DailyTime   string    `json:"daily_time,omitempty"`   // daily："HH:MM"（本地时区）

	CreatedAt time.Time `json:"created_at"`
	LastRun   time.Time `json:"last_run,omitempty"`
	Done      bool      `json:"done,omitempty"` // once 已派发 / 已取消 / 已过期
	Note      string    `json:"note,omitempty"` // 最近一次执行结果或状态说明
	DoneAt    time.Time `json:"done_at,omitempty"`
}

// nextRun 是既有调用点与测试的入口，等价于 nextAt。
func (t *Task) nextRun(now time.Time) time.Time { return t.nextAt(now) }

// dueAt 报告 now 时刻任务是否到期。Done（已完成/已取消）统一先返回 false。
func (t *Task) dueAt(now time.Time) bool {
	if t.Done {
		return false
	}
	switch t.Type {
	case typeOnce:
		return !t.At.IsZero() && !t.At.After(now)
	case typeInterval:
		at := t.intervalAt()
		return !at.IsZero() && !at.After(now)
	case typeDaily:
		occ, ok := t.dailyAt(now)
		if !ok || occ.After(now) {
			return false
		}
		return t.LastRun.IsZero() || t.LastRun.Before(occ)
	}
	return false
}

// nextAt 返回 >= now 的最近一次计划时刻；零值表示不再执行。
// 与 dueAt 分开：nextAt 负责预测（可能在未来），dueAt 负责判断此刻是否该跑。
func (t *Task) nextAt(now time.Time) time.Time {
	if t.Done {
		return time.Time{}
	}
	switch t.Type {
	case typeOnce:
		if t.At.IsZero() {
			return time.Time{}
		}
		if !t.At.After(now) {
			return now
		}
		return t.At
	case typeInterval:
		at := t.intervalAt()
		if at.IsZero() {
			return time.Time{}
		}
		if !at.After(now) {
			return now
		}
		return at
	case typeDaily:
		occ, ok := t.dailyAt(now)
		if !ok {
			return time.Time{}
		}
		if !occ.After(now) {
			if t.LastRun.IsZero() || t.LastRun.Before(occ) {
				return now // 今天这次还没跑，现在就是它的到期时刻
			}
			return occ.AddDate(0, 0, 1)
		}
		return occ
	}
	return time.Time{}
}

// intervalAt 返回周期任务的下一次计划时刻。
func (t *Task) intervalAt() time.Time {
	base := t.LastRun
	if base.IsZero() {
		base = t.CreatedAt
	}
	if base.IsZero() || t.IntervalMin < 1 {
		return time.Time{}
	}
	return base.Add(time.Duration(t.IntervalMin) * time.Minute)
}

// dailyAt 返回 now 所在日期的定点时刻。
func (t *Task) dailyAt(now time.Time) (time.Time, bool) {
	hh, mm, err := parseDaily("", t.DailyTime) // 只看成功与否，报错被丢弃
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location()), true
}

// describe 返回任务的一句话中文描述，供列表与创建回执使用。
func (t *Task) describe(lang string) string {
	var when string
	switch t.Type {
	case typeOnce:
		when = prompt.Tf(lang, "prompt.scheduler.when.once", "%s 执行一次",
			t.At.Format("2006-01-02 15:04"))
	case typeInterval:
		when = prompt.Tf(lang, "prompt.scheduler.when.interval", "每 %d 分钟", t.IntervalMin)
	case typeDaily:
		when = prompt.Tf(lang, "prompt.scheduler.when.daily", "每天 %s", t.DailyTime)
	}
	status := ""
	if t.Done {
		status = prompt.T(lang, "prompt.scheduler.status.done", "（已结束")
		if t.Note != "" {
			status += prompt.Tf(lang, "prompt.scheduler.status.note", "：%s", t.Note)
		}
		status += prompt.T(lang, "prompt.scheduler.status.close", "）")
	} else if t.Note != "" {
		status = prompt.Tf(lang, "prompt.scheduler.status.plain", "（%s）", t.Note)
	}
	return prompt.Tf(lang, "prompt.scheduler.describe", "[%s] %s：%s%s", t.ID, when, t.Content, status)
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// parseDaily 解析 "HH:MM"。
func parseDaily(lang, s string) (hh, mm int, err error) {
	if _, e := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &hh, &mm); e != nil {
		return 0, 0, fmt.Errorf("%s", prompt.T(lang, "prompt.scheduler.err.bad_daily",
			"时间格式应为 HH:MM，例如 08:30"))
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf(prompt.T(lang, "prompt.scheduler.err.bad_time", "时间 %q 不合法"), s)
	}
	return hh, mm, nil
}

// parseAt 解析一次性任务的执行时刻，接受 "2006-01-02 15:04"、
// "2006-01-02 15:04:05" 与 RFC3339。无时区时间按 loc（调用方的「现在」所在地）
// 解析，评测里因此落在虚拟时间线上而不是系统时区。
func parseAt(lang, s string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.Local
	}
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf(prompt.T(lang, "prompt.scheduler.err.bad_at",
		"无法解析时间 %q，请用 2006-01-02 15:04 格式"), s)
}

func newTaskID(now time.Time) string {
	buf := make([]byte, 3)
	_, _ = rand.Read(buf)
	return "t" + now.Format("0102150405") + hex.EncodeToString(buf)
}

// ---------- 持久化（tasks.json，tmp+rename 原子写） ----------

func (p *Plugin) tasksPath() string { return filepath.Join(p.stateDir, "tasks.json") }

func (p *Plugin) loadLocked() error {
	p.tasks = nil
	raw, err := os.ReadFile(p.tasksPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(raw, &p.tasks)
}

// saveLocked 持久化任务表，顺带清理保留期已过的已完成任务。
func (p *Plugin) saveLocked() error {
	kept := p.tasks[:0]
	now := p.nowTime()
	for _, t := range p.tasks {
		if t.Done && !t.DoneAt.IsZero() && now.Sub(t.DoneAt) > doneKeep {
			continue
		}
		kept = append(kept, t)
	}
	p.tasks = kept

	if err := os.MkdirAll(p.stateDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p.tasks, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.tasksPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return atomicfile.Rename(tmp, p.tasksPath())
}

// catchUpLocked 处理宕机期间错过的任务：once 未超过补跑窗口的保持待跑（到点判定
// 自然会立即触发），太久的标记过期；周期任务无需处理，下一个周期自然到来。
func (p *Plugin) catchUpAt(now time.Time) {
	changed := false
	for _, t := range p.tasks {
		if t.Done || t.Type != typeOnce {
			continue
		}
		if now.Sub(t.At) > missedOnceMax {
			t.Done = true
			t.DoneAt = now
			t.Note = prompt.T(p.lang, "prompt.scheduler.note.expired", "错过执行时间过久，已标记过期")
			changed = true
		}
	}
	if changed {
		_ = p.saveLocked()
	}
}

// catchUpLocked 是既有入口：按当前时钟处理宕机期间错过的任务。
func (p *Plugin) catchUpLocked() { p.catchUpAt(p.nowTime()) }
