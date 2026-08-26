package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&readTool{p: p}, &listTool{p: p}}
}

// ---------- read_skill ----------

type readTool struct{ p *Plugin }

func (t *readTool) Name() string { return "read_skill" }

func (t *readTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.skills.tool.read_skill.desc", readDescZH)
}

const readDescZH = "读取一个技能手册的完整内容。已安装的技能及其用途列在系统提示词的技能清单里，" +
	"手头的任务与某条用途相符时，先用这个工具读出它的步骤再动手。"

func (t *readTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.skills.tool.read_skill.schema", readSchemaZH))
}

const readSchemaZH = `{
	"type": "object",
	"properties": {
		"name": {"type": "string", "description": "技能名称，取自技能清单"}
	},
	"required": ["name"]
}`

func (t *readTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	lang := t.p.langOf()
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.skills.err.need_name", "需要提供技能名称"))
	}

	t.p.mu.RLock()
	s, ok := t.p.index[name]
	maxBytes := t.p.maxBytes
	names := make([]string, 0, len(t.p.list))
	for _, it := range t.p.list {
		names = append(names, it.Name)
	}
	t.p.mu.RUnlock()

	if !ok {
		if len(names) == 0 {
			return "", fmt.Errorf(prompt.T(lang, "prompt.skills.err.none_installed",
				"没有名为 %q 的技能，当前一个技能都没有安装"), name)
		}
		// 候选名也要有上限：装了几百个技能时，一次拼错就会灌进去一大段
		hint := names
		if len(hint) > 20 {
			hint = hint[:20]
		}
		msg := prompt.Tf(lang, "prompt.skills.err.no_such", "没有名为 %q 的技能。已安装的有：%s",
			name, strings.Join(hint, prompt.T(lang, "prompt.skills.name_sep", "、")))
		if len(hint) < len(names) {
			msg += prompt.Tf(lang, "prompt.skills.err.no_such_more",
				"（另有 %d 个，用 list_skills 看完整清单）", len(names)-len(hint))
		}
		return "", fmt.Errorf("%s", msg)
	}

	data, err := os.ReadFile(s.File)
	if err != nil {
		return "", fmt.Errorf(prompt.T(lang, "prompt.skills.err.read", "读取技能 %q 失败: %w"), name, err)
	}
	_, body, _ := splitFrontMatter(string(data))

	var b strings.Builder
	b.WriteString(prompt.Tf(lang, "prompt.skills.read.title", "技能「%s」\n", s.Name))
	// 附上所在目录：手册里常会提到随附的脚本或模板，模型得知道去哪儿找它们
	b.WriteString(prompt.Tf(lang, "prompt.skills.read.dir",
		"所在目录：%s（手册中提到的随附文件都在这个目录下）\n\n", s.Dir))

	out, truncated := plugin.TruncateUTF8([]byte(body), maxBytes)
	b.Write(out)
	if truncated {
		b.WriteString(prompt.Tf(lang, "prompt.skills.read.truncated",
			"\n...[内容超过 %d 字节，已截断]", maxBytes))
	}
	return b.String(), nil
}

// ---------- list_skills ----------

type listTool struct{ p *Plugin }

func (t *listTool) Name() string { return "list_skills" }

func (t *listTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.skills.tool.list_skills.desc", listDescZH)
}

const listDescZH = "列出全部已安装的技能及其用途。系统提示词里的技能清单被截断时，用它看到完整的一份。"

func (t *listTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

// Execute 只列出加载成功的技能。扫描时发现的问题（写坏的 frontmatter、缺文件的目录）
// 不进这里：模型对它们无能为力，那是用户在设置页要修的东西。
func (t *listTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	t.p.mu.RLock()
	list := make([]skill, len(t.p.list))
	copy(list, t.p.list)
	lang, dir := t.p.lang, t.p.dir
	t.p.mu.RUnlock()

	if len(list) == 0 {
		return prompt.Tf(lang, "prompt.skills.list.empty", "技能目录 %s 下还没有可用的技能。", dir), nil
	}
	var b strings.Builder
	b.WriteString(prompt.Tf(lang, "prompt.skills.list.count", "共 %d 个技能：\n", len(list)))
	for _, s := range list {
		fmt.Fprintf(&b, "- %s：%s\n", s.Name, s.Desc)
	}
	b.WriteString(prompt.T(lang, "prompt.skills.list.tail", "\n用 read_skill 读取其中任意一个的完整内容。"))
	return b.String(), nil
}
