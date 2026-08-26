package relationship

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// errNotReady 在插件未取得持久化目录时返回，正常流程下不会出现。
func errNotReady(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.relationship.not_ready", "关系状态尚未就绪"))
}

type updateTool struct{ p *Plugin }

func (t *updateTool) Name() string { return "update_relationship" }

func (t *updateTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.relationship.tool.update_relationship.desc", updateDescZH)
}

const updateDescZH = "更新与对方的关系状态。只传发生了变化的字段：非空值覆盖该项，空串清除该项，没提到的字段保持原样；" +
	"bonds 整体覆盖，传空数组清空。只在关系真的变了时调用（表白、吵架、和好、称呼变了、定下新的默契或禁区），不是每轮都调。"

func (t *updateTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.relationship.tool.update_relationship.schema", updateSchemaZH))
}

const updateSchemaZH = `{
	"type": "object",
	"properties": {
		"stage": {"type": "string", "description": "关系阶段，一个短语，如「刚认识」「暧昧中」「热恋」「老夫老妻」「冷战中」（10 字内）"},
		"my_call": {"type": "string", "description": "你怎么叫对方（10 字内）"},
		"their_call": {"type": "string", "description": "对方怎么叫你（10 字内）"},
		"recent": {"type": "string", "description": "最近一次关系上的变动，一句话，如「为忘了纪念日吵过一架，昨天和好了」（60 字内）；记录时刻会一并记下"},
		"bonds": {"type": "array", "items": {"type": "string"}, "description": "相处出来的默契与不碰的禁区，每条一句（30 字内），最多 5 条；整体覆盖，传空数组清空"},
		"their_now": {"type": "string", "description": "对方的近况，一句话，如「项目上线前在赶工」（40 字内）"}
	}
}`

func (t *updateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// 指针区分「没传」与「传了空值」：前者保持原样，后者是清除
	var a struct {
		Stage     *string   `json:"stage"`
		MyCall    *string   `json:"my_call"`
		TheirCall *string   `json:"their_call"`
		Recent    *string   `json:"recent"`
		Bonds     *[]string `json:"bonds"`
		TheirNow  *string   `json:"their_now"`
	}
	lang := t.p.langOf()
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	if a.Stage == nil && a.MyCall == nil && a.TheirCall == nil && a.Recent == nil && a.Bonds == nil && a.TheirNow == nil {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.relationship.result.no_field", "至少要传一个字段"))
	}

	// 快照只读写本轮的写入域：关系属于人格，两条故事线各有各的关系
	store := t.p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", errNotReady(lang)
	}
	updated, cleared, err := store.Apply(Update{
		Stage: a.Stage, MyCall: a.MyCall, TheirCall: a.TheirCall,
		Recent: a.Recent, Bonds: a.Bonds, TheirNow: a.TheirNow,
	}, time.Now())
	if err != nil {
		return "", err
	}

	var parts []string
	sep := prompt.T(lang, "prompt.relationship.result.field_sep", "、")
	if len(updated) > 0 {
		parts = append(parts, prompt.Tf(lang, "prompt.relationship.result.updated", "已更新：%s", strings.Join(updated, sep)))
	}
	if len(cleared) > 0 {
		parts = append(parts, prompt.Tf(lang, "prompt.relationship.result.cleared", "已清除：%s", strings.Join(cleared, sep)))
	}
	if len(parts) == 0 {
		return prompt.T(lang, "prompt.relationship.result.no_change", "没有实际变化。"), nil
	}
	return prompt.Tf(lang, "prompt.relationship.result.joined", "%s。",
		strings.Join(parts, prompt.T(lang, "prompt.relationship.result.part_sep", "；"))), nil
}
