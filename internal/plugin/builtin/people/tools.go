package people

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// errNotReady 在插件未取得持久化目录时返回，正常流程下不会出现。
func errNotReady(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.people.result.not_ready", "人物库尚未就绪"))
}

// maxListBytes 限制 list_people 的返回长度。
const maxListBytes = 16 * 1024

// ---------- upsert_person ----------

type upsertTool struct{ p *Plugin }

func (t *upsertTool) Name() string { return "upsert_person" }

func (t *upsertTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.people.tool.upsert_person.desc",
		"登记或更新角色生活里的一个人物（朋友、同事、家人、常去店里的老板）。"+
			"新人物必须先登记，之后才能出现在故事与日程里；更新时只传要改的字段。"+
			"与某人有过一次实际来往后传 met_now 记下这次来往。对方本人不登记在这里。")
}

func (t *upsertTool) Schema() json.RawMessage {
	lang := t.p.langOf()
	return json.RawMessage(prompt.Tf(lang, "prompt.people.tool.upsert_person.schema",
		upsertpersonSchemaZH, closenessLevels.JSON(lang)))
}

const upsertpersonSchemaZH = `{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "名字或称呼，库内唯一（20 字内）"},
			"relation": {"type": "string", "description": "关系，一句话，如「高中同学」「楼下咖啡店老板」（40 字内）；新人物必填"},
			"profile": {"type": "string", "description": "几句设定：性格、近况、与角色相处的方式（200 字内）。有变化时传完整的新版本"},
			"closeness": {"type": "string", "enum": %s, "description": "亲近度；新人物不填默认「认识」"},
			"met_now": {"type": "boolean", "description": "这次调用是否代表刚与此人有过一次来往（见面、通话、一起做了件事）；为 true 时把此刻记为上次来往"},
			"last_note": {"type": "string", "description": "上次来往的一句摘要（60 字内），与 met_now 一起传"}
		},
		"required": ["name"]
	}`

func (t *upsertTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name      string  `json:"name"`
		Relation  *string `json:"relation"`
		Profile   *string `json:"profile"`
		Closeness *string `json:"closeness"`
		MetNow    bool    `json:"met_now"`
		LastNote  *string `json:"last_note"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}
	s := t.p.snapshot()
	if s.base == "" {
		return "", errNotReady(s.lang)
	}

	// 已存在的人写回他所在的库（同一个人不因人格切换分叉成两份）；
	// 不存在时在本轮写入域新建。定位按可读域做，报错也只报可读的人。
	ps, err := t.p.visiblePeople(ctx)
	if err != nil {
		return "", err
	}
	store := t.p.writeStore(ctx)
	if i := personIndex(ps, a.Name); i >= 0 {
		store = t.p.storeFor(ps[i].Domain)
	}
	if store == nil {
		return "", errNotReady(s.lang)
	}

	res, err := store.Upsert(Update{
		Name: a.Name, Relation: a.Relation, Profile: a.Profile,
		Closeness: a.Closeness, MetNow: a.MetNow, LastNote: a.LastNote,
	}, time.Now(), Limits{MaxPeople: s.maxPeople})
	if err != nil {
		return "", err
	}

	// 回显本次变化：本轮的清单注入在对话开始时就已定型，回执让变化立刻可见。
	if res.Created {
		log.Printf("people: 登记 %s", res.Name)
		line := prompt.Tf(s.lang, "prompt.people.result.created", "已登记 %s（%s，%s）。",
			res.Name, closenessOf(s.lang, a.Closeness), trimmed(a.Relation))
		if a.MetNow {
			line += prompt.T(s.lang, "prompt.people.result.met_now", "上次来往记为此刻。")
		}
		return line, nil
	}
	changeSep := prompt.T(s.lang, "prompt.people.change_sep", "；")
	if len(res.Changes) == 0 {
		return prompt.Tf(s.lang, "prompt.people.result.no_change", "%s 的记录没有变化。", res.Name), nil
	}
	log.Printf("people: 更新 %s（%s）", res.Name, strings.Join(res.Changes, changeSep))
	return prompt.Tf(s.lang, "prompt.people.result.updated", "已更新 %s：%s。",
		res.Name, strings.Join(res.Changes, changeSep)), nil
}

// closenessOf 给出回执里那一档亲近度的说法。入参是模型报来的字面量，可能是任意
// 语言，也可能没报——没报时用默认档。
func closenessOf(lang string, v *string) string {
	if v == nil || *v == "" {
		return closenessLevels.For(lang, defaultCloseness)
	}
	zh, ok := closenessLevels.Canonical(*v)
	if !ok {
		return *v
	}
	return closenessLevels.For(lang, zh)
}

// ---------- list_people ----------

type listTool struct{ p *Plugin }

func (t *listTool) Name() string { return "list_people" }

func (t *listTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.people.tool.list_people.desc",
		"列出人物库的完整内容（含每个人的设定与上次来往）。可按关键词过滤，"+
			"用于清单未完整注入、或要看某人的设定与近况时。")
}

func (t *listTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.people.tool.list_people.schema",
		listpeopleSchemaZH))
}

const listpeopleSchemaZH = `{
		"type": "object",
		"properties": {
			"keyword": {"type": "string", "description": "过滤关键词，对名字、关系、设定与上次来往做包含匹配；留空表示不过滤"}
		}
	}`

func (t *listTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Keyword string `json:"keyword"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", plugin.BadArgs(t.p.langOf(), err)
		}
	}
	s := t.p.snapshot()
	if s.base == "" {
		return "", errNotReady(s.lang)
	}
	// 只数、只列本轮可读的人：人数本身也会泄漏「存在谁」
	ps, err := t.p.visiblePeople(ctx)
	if err != nil {
		return "", err
	}
	if len(ps) == 0 {
		return prompt.T(s.lang, "prompt.people.result.empty", "人物库还是空的，还没有登记任何人。"), nil
	}

	kw := strings.ToLower(strings.TrimSpace(a.Keyword))
	now := time.Now()
	var b strings.Builder
	matched := 0
	for _, p := range ps {
		if kw != "" && !strings.Contains(strings.ToLower(p.Name+"\n"+p.Relation+"\n"+p.Profile+"\n"+p.LastNote), kw) {
			continue
		}
		matched++
		fmt.Fprintf(&b, prompt.T(s.lang, "prompt.people.list.line", "- %s｜%s｜%s"),
			p.Name, closenessLevels.For(s.lang, p.Closeness), p.Relation)
		if a := agoText(s.lang, p.LastMet, now); a != "" {
			fmt.Fprintf(&b, prompt.T(s.lang, "prompt.people.list.last_met", "｜上次来往 %s"), a)
			if p.LastNote != "" {
				b.WriteString(prompt.Tf(s.lang, "prompt.people.render.last_note", "：%s", p.LastNote))
			}
		}
		b.WriteString("\n")
		if p.Profile != "" {
			b.WriteString("  " + p.Profile + "\n")
		}
	}
	if matched == 0 {
		return prompt.Tf(s.lang, "prompt.people.result.no_match", "共 %d 人，没有符合条件的。", len(ps)), nil
	}
	head := prompt.Tf(s.lang, "prompt.people.list.head", "共 %d 人：", len(ps)) + "\n"
	if kw != "" {
		head = prompt.Tf(s.lang, "prompt.people.list.head_filtered", "共 %d 人，符合条件的 %d 人：",
			len(ps), matched) + "\n"
	}
	out, truncated := plugin.TruncateUTF8([]byte(head+strings.TrimRight(b.String(), "\n")), maxListBytes)
	res := string(out)
	if truncated {
		res += prompt.Tf(s.lang, "prompt.people.result.truncated",
			"\n...[超过 %d 字节，已截断，请用关键词过滤]", maxListBytes)
	}
	return res, nil
}

// ---------- remove_person ----------

type removeTool struct{ p *Plugin }

func (t *removeTool) Name() string { return "remove_person" }

func (t *removeTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.people.tool.remove_person.desc",
		"把一个人从人物库移除：搬走了、断了联系、再也不会出现。只是关系变淡不移除，改亲近度即可。")
}

func (t *removeTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.people.tool.remove_person.schema",
		removepersonSchemaZH))
}

const removepersonSchemaZH = `{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "要移除的人"},
			"reason": {"type": "string", "description": "移除的缘由，一句话"}
		},
		"required": ["name", "reason"]
	}`

func (t *removeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.langOf(), err)
	}
	s := t.p.snapshot()
	if strings.TrimSpace(a.Reason) == "" {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.people.result.reason_required",
			"reason 不能为空：一句话说明为什么移除"))
	}
	if s.base == "" {
		return "", errNotReady(s.lang)
	}
	ps, err := t.p.visiblePeople(ctx)
	if err != nil {
		return "", err
	}
	i := personIndex(ps, a.Name)
	if i < 0 {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.people.result.no_such", "没有叫 %q 的人物，现有：%s"),
			strings.TrimSpace(a.Name), nameList(s.lang, names(ps)))
	}
	store := t.p.storeFor(ps[i].Domain)
	if store == nil {
		return "", errNotReady(s.lang)
	}
	removed, err := store.Remove(a.Name)
	if err != nil {
		return "", err
	}
	log.Printf("people: 移除 %s（%s）", removed, strings.TrimSpace(a.Reason))
	return prompt.Tf(s.lang, "prompt.people.result.removed", "已移除 %s。", removed), nil
}
