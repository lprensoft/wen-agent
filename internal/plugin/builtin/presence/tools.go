package presence

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
	return fmt.Errorf("%s", prompt.T(lang, "prompt.presence.not_ready", "现场状态尚未就绪"))
}

type updateTool struct{ p *Plugin }

func (t *updateTool) Name() string { return "update_presence" }

func (t *updateTool) Description() string {
	return prompt.T(t.p.snapshotLang(), "prompt.presence.tool.update_presence.desc",
		"更新现场快照。只传发生了变化的字段：非空值覆盖该项，空串清除该项，没提到的字段保持原样。"+
			"现场变化时（换了地方、脱了外套、姿势变了）随手更新，一轮至多一次。")
}

const updateSchemaZH = `{
		"type": "object",
		"properties": {
			"location": {"type": "string", "description": "所在地点，如「卧室的床边」（60 字内）"},
			"attire": {"type": "string", "description": "穿着，如「睡裙，外套搭在椅背上」（120 字内）"},
			"posture": {"type": "string", "description": "姿态与相对位置，如「靠着床头坐，腿上搭着毯子」（120 字内）"},
			"activity": {"type": "string", "description": "正在做的事，如「翻一本没看完的书」（120 字内）"},
			"effects": {"type": "string", "description": "持续生效的状态，如「只开了床头灯；窗开着一条缝」（200 字内）"},
			"focus": {"type": "string", "description": "当前感官焦点，如「门口的动静」（80 字内）"}
		}
	}`

func (t *updateTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.snapshotLang(), "prompt.presence.tool.update_presence.schema", updateSchemaZH))
}

func (t *updateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// 指针区分「没传」与「传了空串」：前者保持原样，后者是清除
	var a struct {
		Location *string `json:"location"`
		Attire   *string `json:"attire"`
		Posture  *string `json:"posture"`
		Activity *string `json:"activity"`
		Effects  *string `json:"effects"`
		Focus    *string `json:"focus"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(t.p.snapshotLang(), err)
	}
	changes := map[string]string{}
	for key, v := range map[string]*string{
		"location": a.Location, "attire": a.Attire, "posture": a.Posture,
		"activity": a.Activity, "effects": a.Effects, "focus": a.Focus,
	} {
		if v != nil {
			changes[key] = *v
		}
	}
	if len(changes) == 0 {
		return "", fmt.Errorf("%s", prompt.T(t.p.snapshotLang(), "prompt.presence.need_field", "至少要传一个字段"))
	}

	// 快照只读写本轮的写入域：两条故事线各有各的现场
	store := t.p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", errNotReady(t.p.snapshotLang())
	}
	updated, cleared, err := store.Apply(changes, time.Now())
	if err != nil {
		return "", err
	}

	var parts []string
	if len(updated) > 0 {
		parts = append(parts, prompt.Tf(t.p.snapshotLang(), "prompt.presence.result.updated", "已更新：%s",
			strings.Join(updated, prompt.T(t.p.snapshotLang(), "prompt.presence.result.sep", "、"))))
	}
	if len(cleared) > 0 {
		parts = append(parts, prompt.Tf(t.p.snapshotLang(), "prompt.presence.result.cleared", "已清除：%s",
			strings.Join(cleared, prompt.T(t.p.snapshotLang(), "prompt.presence.result.sep", "、"))))
	}
	if len(parts) == 0 {
		return prompt.T(t.p.snapshotLang(), "prompt.presence.result.no_change", "没有实际变化。"), nil
	}
	return strings.Join(parts, prompt.T(t.p.snapshotLang(), "prompt.presence.result.join", "；")) +
		prompt.T(t.p.snapshotLang(), "prompt.presence.result.period", "。"), nil
}
