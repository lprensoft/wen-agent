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

// nextRun 返回任务的下一次到期时刻；零值表示不再执行。
func (t *Task) nextRun(now time.Time) time.Time {
	if t.Done {
		return time.Time{}
	}
	switch t.Type {
	case typeOnce:
		return t.At
	case typeInterval:
		base := t.LastRun
		if base.IsZero() {
			base = t.CreatedAt
		}
		return base.Add(time.Duration(t.IntervalMin) * time.Minute)
	case typeDaily:
		hh, mm, err := parseDaily("", t.DailyTime) // 只看成功与否，报错被丢弃
		if err != nil {
			return time.Time{}
		}
		next := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
		// 今天的点已过、或今天已经跑过，则排到明天
		if !next.After(now) || (!t.LastRun.IsZero() && sameDay(t.LastRun, next)) {
			next = next.AddDate(0, 0, 1)
		}
		return next
	}
	return time.Time{}
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

// parseAt 解析一次性任务的执行时刻（本地时区），接受 "2006-01-02 15:04"、
// "2006-01-02 15:04:05" 与 RFC3339。
func parseAt(lang, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf(prompt.T(lang, "prompt.scheduler.err.bad_at",
		"无法解析时间 %q，请用 2006-01-02 15:04 格式"), s)
}

func newTaskID() string {
	buf := make([]byte, 3)
	_, _ = rand.Read(buf)
	return "t" + time.Now().Format("0102150405") + hex.EncodeToString(buf)
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
	for _, t := range p.tasks {
		if t.Done && !t.DoneAt.IsZero() && time.Since(t.DoneAt) > doneKeep {
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
	return os.Rename(tmp, p.tasksPath())
}

// catchUpLocked 处理宕机期间错过的任务：once 未超过补跑窗口的保持待跑（到点判定
// 自然会立即触发），太久的标记过期；周期任务无需处理，下一个周期自然到来。
func (p *Plugin) catchUpLocked() {
	now := time.Now()
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
