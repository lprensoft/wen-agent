package phone

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// readTool 细看刷到的一条：只给订阅源里本来就有的摘要，不再联网——「点进去看全文」
// 是另一件事，开着 web_fetch 的话模型自己会去。
type readTool struct{ p *Plugin }

func (t *readTool) Name() string { return "read_news" }

func (t *readTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.phone.tool.read_news.desc", readDescZH)
}

const readDescZH = "细看手机上刷到的一条消息：给出来源、时间、摘要与链接。按标题里的几个字找。"

func (t *readTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.phone.tool.read_news.schema", readSchemaZH))
}

const readSchemaZH = `{
	"type": "object",
	"properties": {
		"title": {"type": "string", "description": "那条消息标题里的几个字"}
	},
	"required": ["title"]
}`

func (t *readTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Title string `json:"title"`
	}
	lang := t.p.langOf()
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	q := strings.ToLower(strings.Trim(strings.TrimSpace(a.Title), "「」“”\""))
	t.p.dataMu.RLock()
	items := append([]Item(nil), t.p.st.Items...)
	t.p.dataMu.RUnlock()
	if len(items) == 0 {
		return "", fmt.Errorf("%s", prompt.T(lang, "prompt.phone.err.nothing", "手机上还没刷到任何消息。"))
	}
	// 条目按刷到的先后排，新的在前：标题片段对上好几条时取最近刷到的那条
	for _, it := range items {
		if q != "" && strings.Contains(strings.ToLower(it.Title), q) {
			summary := it.Summary
			if summary == "" {
				summary = prompt.T(lang, "prompt.phone.read.no_summary", "（这条没有摘要）")
			}
			out := prompt.Tf(lang, "prompt.phone.read.result", "「%s」（%s，%s）\n\n%s",
				it.Title, it.Source, agoText(lang, it.when(), t.p.now()), summary)
			if it.Link != "" {
				out += prompt.Tf(lang, "prompt.phone.read.link", "\n\n链接：%s", it.Link)
			}
			return out, nil
		}
	}
	var titles []string
	for i, it := range items {
		if i == 5 {
			break
		}
		titles = append(titles, prompt.Tf(lang, "prompt.phone.render.title", "「%s」", it.Title))
	}
	return "", fmt.Errorf(prompt.T(lang, "prompt.phone.err.no_such", "手机上没刷到标题含「%s」的消息。最近刷到的：%s"),
		a.Title, strings.Join(titles, prompt.T(lang, "prompt.phone.render.sep", "、")))
}
