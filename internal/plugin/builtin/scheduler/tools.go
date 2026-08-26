package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// scheduleTool 创建定时任务。
type scheduleTool struct{ p *Plugin }

func (t *scheduleTool) Name() string { return "schedule_task" }

func (t *scheduleTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.scheduler.tool.schedule_task.desc", scheduleDescZH)
}

const scheduleDescZH = "创建一个定时任务，到点后系统会以任务内容自动发起一轮对话并执行。" +
	"支持三种类型：once（指定时刻执行一次）、interval（每隔 N 分钟）、daily（每天定点）。" +
	"任务内容应写成给助手的完整指令，因为执行时没有当前对话的上下文。"

func (t *scheduleTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.scheduler.tool.schedule_task.schema", scheduleSchemaZH))
}

const scheduleSchemaZH = `{
  "type": "object",
  "properties": {
    "content": {"type": "string", "description": "到点后要执行的指令内容，需自包含、不依赖当前对话上下文"},
    "schedule_type": {"type": "string", "enum": ["once", "interval", "daily"], "description": "任务类型：once 一次性；interval 周期；daily 每天定点"},
    "at": {"type": "string", "description": "once 必填：执行时刻，格式 2006-01-02 15:04（本地时区）"},
    "interval_minutes": {"type": "integer", "minimum": 1, "description": "interval 必填：周期分钟数"},
    "daily_time": {"type": "string", "description": "daily 必填：每天的执行时间，格式 HH:MM（本地时区）"}
  },
  "required": ["content", "schedule_type"]
}`

func (t *scheduleTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Content         string `json:"content"`
		ScheduleType    string `json:"schedule_type"`
		At              string `json:"at"`
		IntervalMinutes int    `json:"interval_minutes"`
		DailyTime       string `json:"daily_time"`
	}
	lang := t.p.langOf()
	if err := json.Unmarshal(args, &req); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	if strings.TrimSpace(req.Content) == "" {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.scheduler.err.empty_content", "任务内容不能为空"))
	}

	task := &Task{
		ID:        newTaskID(),
		Content:   strings.TrimSpace(req.Content),
		Type:      req.ScheduleType,
		CreatedAt: time.Now(),
	}
	switch req.ScheduleType {
	case typeOnce:
		at, err := parseAt(lang, req.At)
		if err != nil {
			return "", err
		}
		if !at.After(time.Now()) {
			return "", fmt.Errorf(prompt.T(lang, "prompt.scheduler.err.past", "执行时刻 %s 已经过去"), at.Format("2006-01-02 15:04"))
		}
		task.At = at
	case typeInterval:
		if req.IntervalMinutes < 1 {
			return "", fmt.Errorf("%s", prompt.T(lang, "prompt.scheduler.err.bad_interval", "interval 类型需要 interval_minutes ≥ 1"))
		}
		task.IntervalMin = req.IntervalMinutes
	case typeDaily:
		if _, _, err := parseDaily(lang, req.DailyTime); err != nil {
			return "", err
		}
		task.DailyTime = strings.TrimSpace(req.DailyTime)
	default:
		return "", fmt.Errorf(prompt.T(lang, "prompt.scheduler.err.bad_type", "未知任务类型 %q，可选 once / interval / daily"), req.ScheduleType)
	}

	p := t.p
	p.mu.Lock()
	active := 0
	for _, x := range p.tasks {
		if !x.Done {
			active++
		}
	}
	if active >= maxTasks {
		p.mu.Unlock()
		return "", fmt.Errorf(prompt.T(lang, "prompt.scheduler.err.too_many", "未完成任务已达上限 %d，请先取消一些"), maxTasks)
	}
	p.tasks = append(p.tasks, task)
	err := p.saveLocked()
	p.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf(prompt.T(lang, "prompt.scheduler.err.save", "保存任务失败: %w"), err)
	}
	p.wakeup()

	next := task.nextRun(time.Now())
	return prompt.Tf(lang, "prompt.scheduler.result.created", "已创建定时任务 %s，下次执行：%s", task.describe(lang), next.Format("2006-01-02 15:04")), nil
}

// listTool 列出任务。
type listTool struct{ p *Plugin }

func (t *listTool) Name() string { return "list_tasks" }

func (t *listTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.scheduler.tool.list_tasks.desc",
		"列出全部定时任务，包括待执行的与最近 24 小时内结束的。")
}

func (t *listTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *listTool) Execute(context.Context, json.RawMessage) (string, error) {
	p := t.p
	p.mu.Lock()
	defer p.mu.Unlock()
	lang := p.lang
	if len(p.tasks) == 0 {
		return prompt.T(lang, "prompt.scheduler.result.no_tasks", "当前没有定时任务。"), nil
	}
	now := time.Now()
	var b strings.Builder
	for _, task := range p.tasks {
		b.WriteString("- " + task.describe(lang))
		if next := task.nextRun(now); !next.IsZero() {
			b.WriteString(prompt.Tf(lang, "prompt.scheduler.result.next_run", "，下次执行 %s",
				next.Format("2006-01-02 15:04")))
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// cancelTool 取消任务。
type cancelTool struct{ p *Plugin }

func (t *cancelTool) Name() string { return "cancel_task" }

func (t *cancelTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.scheduler.tool.cancel_task.desc",
		"按任务 ID 取消一个定时任务（ID 可用 list_tasks 查到）。")
}

func (t *cancelTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.scheduler.tool.cancel_task.schema", cancelSchemaZH))
}

const cancelSchemaZH = `{
  "type": "object",
  "properties": {"id": {"type": "string", "description": "要取消的任务 ID"}},
  "required": ["id"]
}`

func (t *cancelTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		ID string `json:"id"`
	}
	lang := t.p.langOf()
	if err := json.Unmarshal(args, &req); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	p := t.p
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, task := range p.tasks {
		if task.ID == req.ID {
			if task.Done {
				return prompt.Tf(lang, "prompt.scheduler.result.already_done", "任务 %s 已经结束，无需取消。", task.ID), nil
			}
			task.Done = true
			task.DoneAt = time.Now()
			task.Note = prompt.T(lang, "prompt.scheduler.note.cancelled", "已取消")
			if err := p.saveLocked(); err != nil {
				return "", fmt.Errorf(prompt.T(lang, "prompt.scheduler.err.save_short", "保存失败: %w"), err)
			}
			return prompt.Tf(lang, "prompt.scheduler.result.cancelled", "已取消任务 %s。", task.ID), nil
		}
	}
	return "", fmt.Errorf(prompt.T(lang, "prompt.scheduler.err.not_found", "找不到任务 %q"), req.ID)
}
