package scene

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stateMaxRows 是状态页上最多列几处场景。注入侧有自己的条数上限，这里只兜页面长度。
const stateMaxRows = 30

// State 把各可见域的场景记忆交给设置页的「角色状态」。
//
// 舞台设定是插件配置，不在这里重复；这里只列场景本身，按域分块，其余人格域
// 由界面折叠。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	s := p.snapshot()
	if s.base == "" {
		return nil
	}

	var out []plugin.StateSection
	failed := false
	for _, tag := range plugin.ReadDomains(s.base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		scenes, err := store.List()
		if err != nil {
			i18n.Logf(s.lang, "log.scene.state_failed", "读取场景状态失败（域 %q）: %v", tag, err)
			failed = true
			continue
		}
		if len(scenes) == 0 {
			continue
		}

		rows := make([]plugin.StateRow, 0, min(len(scenes), stateMaxRows))
		var latest time.Time
		for _, sc := range scenes {
			if sc.Updated.After(latest) {
				latest = sc.Updated
			}
			if len(rows) >= stateMaxRows {
				continue
			}
			rows = append(rows, plugin.StateRow{
				Value:  sc.Name,
				Detail: clipText(sc.Detail, 200),
				ID:     sc.Name,
				Edits:  sceneEdits(q.Lang, sc),
			})
		}
		out = append(out, plugin.StateSection{
			Key:       "scenes",
			Title:     i18n.T(q.Lang, "plugin.scene.state.title", "场景"),
			Summary:   i18n.Tf(q.Lang, "plugin.scene.state.count", "%d 处", len(scenes)),
			Rows:      rows,
			Domain:    tag,
			UpdatedAt: latest,
			More:      len(scenes) - len(rows),
			Edits:     sceneAddEdits(q.Lang),
		})
	}
	if len(out) == 0 {
		if failed {
			return nil
		}
		return []plugin.StateSection{{
			Key:   "scenes",
			Title: i18n.T(q.Lang, "plugin.scene.state.title", "场景"),
			Empty: i18n.T(q.Lang, "plugin.scene.state.empty", "还没有记录场景。"),
			Edits: sceneAddEdits(q.Lang),
		}}
	}
	return out
}

// 状态页上的操作标识。静态、不翻译。
const (
	stateSection  = "scenes"
	stateOpEdit   = "edit"
	stateOpDelete = "delete"
	stateOpAdd    = "add"
)

func sceneEdits(lang string, sc Scene) []plugin.StateEdit {
	return []plugin.StateEdit{
		{
			Op:    stateOpEdit,
			Label: i18n.T(lang, "plugin.scene.state.edit", "修改"),
			Fields: []plugin.StateField{{
				Key: "detail", Label: i18n.T(lang, "plugin.scene.state.field.detail", "描述"),
				Type: plugin.StateFieldTextarea, Value: sc.Detail, MaxLen: maxDetailRunes, Required: true,
			}},
		},
		{
			Op:      stateOpDelete,
			Label:   i18n.T(lang, "plugin.scene.state.delete", "删除"),
			Confirm: i18n.T(lang, "plugin.scene.state.delete_confirm", "删除这处场景？之后演绎到那个地方时不再以它为准。"),
			Danger:  true,
		},
	}
}

func sceneAddEdits(lang string) []plugin.StateEdit {
	return []plugin.StateEdit{{
		Op:    stateOpAdd,
		Label: i18n.T(lang, "plugin.scene.state.add", "记录新场景"),
		Fields: []plugin.StateField{
			{
				Key: "name", Label: i18n.T(lang, "plugin.scene.state.field.name", "名称"),
				Type: plugin.StateFieldText, MaxLen: maxNameRunes, Required: true,
			},
			{
				Key: "detail", Label: i18n.T(lang, "plugin.scene.state.field.detail", "描述"),
				Type: plugin.StateFieldTextarea, MaxLen: maxDetailRunes, Required: true,
			},
		},
	}}
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
