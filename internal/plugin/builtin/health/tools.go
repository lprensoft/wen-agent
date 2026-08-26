package health

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// errNotReady 在插件未取得持久化目录时返回，正常流程下不会出现。
func errNotReady(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.health.result.not_ready", "身体状况尚未就绪"))
}

// ---------- set_condition ----------

type setTool struct{ p *Plugin }

func (t *setTool) Name() string { return "set_condition" }

func (t *setTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.health.tool.set_condition.desc",
		"记下角色新添的一条身体小恙：叫什么、最重时多重、几小时后发作、大概几天好、怎么处理。"+
			"记下之后病程由时间自动推进，不需要再手动加重或减轻；要改处理方式、严重度或标记痊愈用 update_condition。"+
			"痊愈后一段时间内、或同时记着的状况已到上限时会被拒绝并说明规则。")
}

func (t *setTool) Schema() json.RawMessage {
	lang := t.p.langOf()
	return json.RawMessage(prompt.Tf(lang, "prompt.health.tool.set_condition.schema",
		setSchemaZH, severities.JSON(lang), careKinds.JSON(lang)))
}

// setSchemaZH 是 set_condition 的参数 Schema。整段一个键：它是手写的 JSON，拆成
// 一个字段一个键的话，译者要照着结构去拼，而拼错一个引号就是一份非法的 Schema。
// 两个占位符是严重度与处理方式的候选项，由 prompt.Enum 按语言拼出。
const setSchemaZH = `{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "状况名，不超过 12 字，如「着凉」「肠胃不舒服」「没睡好的倦怠」"},
			"severity": {"type": "string", "enum": %[1]s, "description": "最重的时候有多重"},
			"onset_hours": {"type": "number", "description": "几小时后开始发作；0 或不填表示此刻就有感觉。下午淋的雨晚上才头疼，就填几个小时。最多 48"},
			"days": {"type": "integer", "description": "硬扛着大概几天好，1 到 14。着凉三到七天，发烧一到三天，头疼、肠胃不适一两天"},
			"care": {"type": "string", "enum": %[2]s, "description": "眼下怎么处理，不填为硬扛。吃药好得快些，就医最快"}
		},
		"required": ["name", "severity", "days"]
	}`

func (t *setTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name       string  `json:"name"`
		Severity   string  `json:"severity"`
		OnsetHours float64 `json:"onset_hours"`
		Days       int     `json:"days"`
		Care       string  `json:"care"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}
	if a.Care == "" {
		a.Care = careTough
	}
	// 报的严重度先换回中文规范值：下面要拿它跟按上限收过的档位比，比的必须是同一种
	// 语言的同一个词，否则英文界面下每次都会多出一句「已按上限收」。
	zhSeverity, _ := severities.Canonical(a.Severity)
	if a.OnsetHours < 0 || math.IsNaN(a.OnsetHours) {
		a.OnsetHours = 0
	}
	clippedOnset := a.OnsetHours > maxOnsetHours
	if clippedOnset {
		a.OnsetHours = maxOnsetHours
	}

	s := t.p.snapshot()
	tag := plugin.ScopeFrom(ctx).Write
	store := t.p.storeFor(tag)
	if store == nil {
		return "", errNotReady(s.lang)
	}
	now := time.Now()
	onset := now.Add(time.Duration(a.OnsetHours * float64(time.Hour)))
	c, err := store.Add(a.Name, a.Severity, onset, a.Days, a.Care, now, s.limits())
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, prompt.T(s.lang, "prompt.health.result.noted", "已记下：%s，"), c.Name)
	if c.Onset.After(now) {
		fmt.Fprintf(&b, prompt.T(s.lang, "prompt.health.result.will_start", "预计%s发作，到时"),
			untilText(s.lang, c.Onset.Sub(now)))
	} else {
		b.WriteString(prompt.T(s.lang, "prompt.health.result.starts_now", "此刻开始有感觉，"))
	}
	fmt.Fprintf(&b, prompt.T(s.lang, "prompt.health.result.peak_days", "最重时%s，硬扛着约 %d 天好（眼下%s）。"),
		band(s.lang, c.Peak), c.Days, careKinds.For(s.lang, c.Care))
	// 拦截生效时要把规则告诉模型，否则它只会换个说法再报一次
	if got := band(s.lang, c.Peak); got != severities.For(s.lang, zhSeverity) {
		fmt.Fprintf(&b, prompt.T(s.lang, "prompt.health.result.severity_capped",
			"严重度按上限收成「%s」（原报「%s」），只演绎日常小恙。"), got, a.Severity)
	}
	if clippedOnset {
		fmt.Fprintf(&b, prompt.T(s.lang, "prompt.health.result.onset_capped",
			"发作时刻最多延后 %d 小时，已按上限收。"), maxOnsetHours)
	}
	return b.String(), nil
}

// ---------- update_condition ----------

type updateTool struct{ p *Plugin }

func (t *updateTool) Name() string { return "update_condition" }

func (t *updateTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.health.tool.update_condition.desc",
		"修改一条已记下的身体状况：换处理方式（硬扛 / 吃药 / 就医，决定好得快慢）、改严重度（明显加重或好转）、或标记痊愈。"+
			"只传要改的项；只有一条状况时可以不传名字。")
}

func (t *updateTool) Schema() json.RawMessage {
	lang := t.p.langOf()
	return json.RawMessage(prompt.Tf(lang, "prompt.health.tool.update_condition.schema",
		updateSchemaZH, careKinds.JSON(lang), severities.JSON(lang)))
}

const updateSchemaZH = `{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "要改哪条状况，与记下时的名字一致；只有一条时可不填"},
			"care": {"type": "string", "enum": %[1]s, "description": "新的处理方式"},
			"severity": {"type": "string", "enum": %[2]s, "description": "此刻的严重度，明显加重或好转时才改"},
			"recovered": {"type": "boolean", "description": "true 表示已经好了，状况从身上移除"}
		}
	}`

func (t *updateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name      string  `json:"name"`
		Care      *string `json:"care"`
		Severity  *string `json:"severity"`
		Recovered bool    `json:"recovered"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}

	s := t.p.snapshot()
	tag := plugin.ScopeFrom(ctx).Write
	store := t.p.storeFor(tag)
	if store == nil {
		return "", errNotReady(s.lang)
	}
	now := time.Now()
	c, changes, err := store.Apply(a.Name, Update{Care: a.Care, Severity: a.Severity, Recovered: a.Recovered}, now, s.limits())
	if err != nil {
		return "", err
	}
	if a.Recovered {
		dropCue(tag, c.ID) // 还没说出口的「开始发作了」不该再说
		return prompt.Tf(s.lang, "prompt.health.result.recovered", "%s：已痊愈。", c.Name), nil
	}

	var b strings.Builder
	b.WriteString(prompt.Tf(s.lang, "prompt.health.result.name_colon", "%s：", c.Name))
	if len(changes) == 0 {
		b.WriteString(prompt.T(s.lang, "prompt.health.result.no_change", "没有变化。"))
	} else {
		b.WriteString(prompt.Tf(s.lang, "prompt.health.result.changes", "%s。",
			strings.Join(changes, prompt.T(s.lang, "prompt.health.change_sep", "；"))))
	}
	v := c.view(now)
	if v.Phase == phasePending {
		fmt.Fprintf(&b, prompt.T(s.lang, "prompt.health.result.still_pending", "还没发作，预计%s发作。"),
			untilText(s.lang, c.Onset.Sub(now)))
	} else {
		fmt.Fprintf(&b, prompt.T(s.lang, "prompt.health.result.now_state", "此刻%s，%s。"),
			band(s.lang, v.Severity), recoverText(s.lang, v.Remaining))
	}
	return b.String(), nil
}
