package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// extractTimeout 是单次提炼的时长上限。后台跑的调用必须有上界，
// 否则一次卡住的请求会把这个会话的提炼窗口一直占着。
const extractTimeout = 2 * time.Minute

// extractPromptZH 让模型从一段对话里挑出值得长期保留的结论，并对照已有记忆做修订与提及标记。
// 刻意要求“没有就返回空数组”——宁可不记，也不要为了凑数造出噪音记忆。
//
// 六类定义必须与 memory.go 的 promptGuideZH 逐句同源。这两份判据一份管当场保存、
// 一份管自动提炼，措辞不一致时同一句话会在两条路径上得到相反的取舍：promptGuide
// 早已改成兼顾生活（纯工程定义让「我早上十点上班」落不进任何一类），这里曾原样
// 留着窄版本，于是生活类内容在自动提炼这条主力路径上一直被挡在门外。
const extractPromptZH = `请从下面这段对话中挑出**值得长期记住**的结论，供之后的对话使用。

只挑这六类：
- 偏好：对方明确表达的、长期有效的喜好与要求——喜欢什么、不喜欢什么、希望怎么
  相处、希望怎么表达
- 约定：两人之间或协作上定下来的规则与安排
- 事实：关于对方及其处境的、不易重新得知的信息——身份、作息、工作、住处、家人、
  健康状况，以及环境、配置、结构这类
- 踩坑：已经验证过的失败原因与正确做法
- 经历：自己做过、遇到过的事——和谁去了哪、生活里发生了什么、独处时做了什么；主语
  是自己而不是对方，这是它与「事实」的分界
- 边界：任何一方明确表达过的不愿意与坚持——拒绝过什么、坚持过什么立场。谁表达的、
  对什么、因为什么都要写进正文；摘要只写对什么事的立场，不复述当时的对话。说出口
  的才算，委婉带开话头的不算；自己表达的边界归这一类，不归「经历」

日常生活里的近况、处境与安排同样属于上面的分类，照样挑出。
不要挑：一次性的任务细节、临时中间结果、可以随时重新读取的内容、尚未确认的猜测。
定在某天某时的单次安排（明天下午三点见面、周六去看展）不存为记忆，归日程，过后由时间线记经历。
宁可少挑也不要凑数；确实没有值得保留的内容时 memories 给空数组 []。

已经保存的记忆如下（每行「分类/标题 — 摘要」）：
%s

对照这份清单办三件事：
1. 新增：清单里没有的新结论，mode 填 create。
2. 修订：对话中出现了**推翻**清单里某条记忆的新说法时，mode 填 replace，name 原样填那条
   记忆的标题，content 写新的结论，并在末尾用一句话交代被推翻的旧结论与大致时间。
   只在确实矛盾时修订：同一件事又被提到一次不算矛盾。也不要给清单里已有的事换个说法
   再新建一条——那会让两条互相打架的记忆同时留在库里。
3. 提及：清单里有哪些记忆在这段对话中被谈到了（无论是否修订），把标题列进 mentioned；
   没有就给空数组 []。
%s
按下面的 JSON 输出，不要输出任何其它文字：
{"memories":[{"name":"简短标题","description":"一句话摘要","type":"%s","content":"完整内容","mode":"create|replace"%s}],"mentioned":["分类/标题"]}

对话：
%s`

// decayGuideZH 只在开启了淡忘时追加。措辞不涉及任何具体场景——一条记忆会不会随时间
// 失去意义是它自身的性质，与产生它的场景无关。
const decayGuideZH = `
每条记忆还要给出 decay：
- true：会随时间失去意义的内容——近况、心情、当下的处境与安排、生活里的片段。
- false：长期有效的内容——稳定的偏好、定下的约定、不易重新得知的事实、验证过的踩坑。
边界类一律填 false：立场的改变要有来由、由修订承载，不由淡忘悄悄发生。
标 true 的记忆久未提及会逐步淡忘，最终移出记忆库；拿不准就填 false。
`

const decaySchemaFieldZH = `,"decay":true|false`

// extracted 是模型返回的一条待保存记忆。
type extracted struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Content     string `json:"content"`
	Mode        string `json:"mode"`
	Decay       bool   `json:"decay"`
}

// extractOutput 是一次提炼调用的完整返回。
type extractOutput struct {
	Memories  []extracted `json:"memories"`
	Mentioned []string    `json:"mentioned"`
}

// extractResult 是一次提炼的落盘结果。
type extractResult struct {
	saved   []Entry // 新增的条目
	revised []Entry // 修订掉的已有条目
	touched int     // 仅刷新了最后使用时间的条数
}

func (r extractResult) empty() bool {
	return len(r.saved) == 0 && len(r.revised) == 0 && r.touched == 0
}

// names 列出本次新增与修订的条目，供日志与压缩注记使用。
func (r extractResult) names(lang string) []string {
	out := make([]string, 0, len(r.saved)+len(r.revised))
	for _, e := range r.saved {
		out = append(out, memTypes.For(lang, e.Type)+"/"+e.Name)
	}
	for _, e := range r.revised {
		out = append(out, prompt.Tf(lang, "prompt.memory.name.revised", "%s/%s（修订）",
			memTypes.For(lang, e.Type), e.Name))
	}
	return out
}

// extractMemories 用一次独立的模型调用从一段对话中提炼记忆并落盘。
//
// ctx 必须已经带上这次提炼所属的可见域：读哪些库、写哪个库全由它决定。
func (p *Plugin) extractMemories(ctx context.Context, s settings, complete plugin.CompleteFunc, dialogue string) (extractResult, error) {
	var res extractResult
	store := p.writeStore(ctx)
	if store == nil {
		return res, errNotReady(s.lang)
	}
	// 已有记忆只取本轮可读范围：跨域取的话，不可读域的标题会莫名压掉本域一条合法的保存
	existing, err := p.visibleEntries(ctx)
	if err != nil {
		return res, err
	}

	decayBlock, decayField := "", ""
	if s.decay {
		decayBlock = prompt.T(s.lang, "prompt.memory.extract.decay_guide", decayGuideZH)
		decayField = prompt.T(s.lang, "prompt.memory.extract.decay_field", decaySchemaFieldZH)
	}
	// 分类的候选串按语言给，与提炼提示词同一门语言——否则模型照着一列中文分类
	// 去填一份英文的判据，回来的值两边都对不上。
	raw, err := complete(ctx, fmt.Sprintf(
		prompt.T(s.lang, "prompt.memory.extract.prompt", extractPromptZH),
		renderExisting(s.lang, existing), decayBlock,
		strings.Join(memTypes.Values(s.lang), "|"), decayField, dialogue))
	if err != nil {
		return res, err
	}
	out, err := parseExtracted(raw)
	if err != nil {
		return res, err
	}

	items := out.Memories
	if len(items) > s.maxExtract {
		items = items[:s.maxExtract]
	}
	for _, it := range items {
		e, revised, err := p.applyExtracted(s, store, existing, it)
		if err != nil {
			continue // 单条非法（如分类取值不对）不影响其余
		}
		if revised {
			res.revised = append(res.revised, e)
		} else {
			res.saved = append(res.saved, e)
		}
	}
	res.touched = p.touchMentioned(s, existing, out.Mentioned, res)
	return res, nil
}

// applyExtracted 落盘一条提炼结果，返回它是新增（false）还是修订（true）。
func (p *Plugin) applyExtracted(s settings, store *Store, existing []Entry, it extracted) (Entry, bool, error) {
	if strings.TrimSpace(it.Name) == "" || strings.TrimSpace(it.Content) == "" {
		return Entry{}, false, fmt.Errorf("标题或内容为空")
	}
	// 先换回中文规范值：下面那条「边界类不淡忘」是按规范值判的，模型报回英文时
	// 直接比会漏掉，一条立场会被标成随时间淡忘。
	zhType, _ := memTypes.Canonical(it.Type)
	e := Entry{
		Name:        it.Name,
		Description: it.Description,
		Type:        it.Type,
		Content:     it.Content,
		// 边界类强制不淡忘：立场的改变要有来由、由修订承载，不能由 decay 无声发生。
		// 只拦自动路径——工具那头人为要存一条会淡忘的边界是合法操作，硬拦会把工具
		// 变成猜不透的黑盒，那里只靠提示词引导。
		Decay: s.decay && it.Decay && zhType != typeBoundary,
	}

	old, exists := findEntry(existing, it.Name)
	if !exists {
		saved, err := store.Save(e, false)
		return saved, false, err
	}
	if it.Mode != "replace" {
		// 已有同名记忆而模型并没打算修订，跳过而不是覆盖
		return Entry{}, false, fmt.Errorf("已存在同名记忆")
	}
	// 修订要落回这条记忆所在的库，而不是本轮的写入域——多可见域下两者可能不同，
	// 写错库的结果是同名记忆在两个库里各留一份，索引合并后只显示其中一份。
	target := p.storeFor(old.Domain)
	if target == nil {
		return Entry{}, false, errNotReady(s.lang)
	}
	e.Name = old.Name // 沿用原标题，避免大小写或分类前缀的差异生成第二条
	saved, err := target.Save(e, true)
	return saved, true, err
}

// touchMentioned 刷新对话中被提及的记忆的最后使用时间，返回实际写盘的条数。
// 本次已新增或修订的条目跳过——它们的时间刚由 Save 更新过。
func (p *Plugin) touchMentioned(s settings, existing []Entry, mentioned []string, res extractResult) int {
	if !s.decay || len(mentioned) == 0 {
		return 0 // 不淡忘就没人读这个时间，不值得为它写盘
	}
	just := map[string]bool{}
	for _, e := range res.saved {
		just[strings.ToLower(e.Name)] = true
	}
	for _, e := range res.revised {
		just[strings.ToLower(e.Name)] = true
	}

	now := time.Now()
	n := 0
	for _, name := range mentioned {
		e, ok := findEntry(existing, name)
		if !ok || just[strings.ToLower(e.Name)] {
			continue
		}
		store := p.storeFor(e.Domain)
		if store == nil {
			continue
		}
		if written, err := store.Touch(e.Name, now); err == nil && written {
			n++
		}
	}
	return n
}

// renderExisting 把已有记忆渲染成给提炼用的清单。带上摘要而不只是标题：只看标题
// 认不出「饮食禁忌」与「喜欢香菜」说的是同一件事，于是两条互相打架的记忆会一起
// 留在库里——那比漏记一条更糟。
func renderExisting(lang string, entries []Entry) string {
	if len(entries) == 0 {
		return prompt.T(lang, "prompt.memory.extract.none_yet", "（暂无）")
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(memTypes.For(lang, e.Type))
		b.WriteString("/")
		b.WriteString(e.Name)
		if e.Description != "" {
			b.WriteString(" — ")
			b.WriteString(e.Description)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// parseExtracted 解析模型输出。模型常把 JSON 包在围栏里或前后带说明文字，因此先剥
// 围栏、再退回到截取最外层的括号。也接受直接给出记忆数组的简化形式。
func parseExtracted(raw string) (extractOutput, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return extractOutput{}, nil
	}
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:] // 跳过 ```json 这类语言标记
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			s = strings.TrimSpace(rest[:j])
		}
	}

	// 由最先出现的括号决定按哪种形式解析。不能反过来先试对象：裸数组的第一个元素
	// 本身就是个对象，先试对象会「成功」解析出一份字段全空的结果，于是一次正常的
	// 提炼被当成什么也没挑出来。
	objAt, arrAt := strings.IndexByte(s, '{'), strings.IndexByte(s, '[')
	try := []func(string) (extractOutput, bool){parseObjectForm, parseArrayForm}
	if arrAt >= 0 && (objAt < 0 || arrAt < objAt) {
		try[0], try[1] = try[1], try[0]
	}
	for _, fn := range try {
		if out, ok := fn(s); ok {
			return out, nil
		}
	}
	return extractOutput{}, fmt.Errorf("提炼结果不是合法的 JSON: %s", truncateRunes(s, 120))
}

// parseObjectForm 解析 {"memories":[...],"mentioned":[...]} 形式。
// 两个字段都缺时判为不匹配——那多半是别的东西恰好也是个 JSON 对象。
func parseObjectForm(s string) (extractOutput, bool) {
	body, ok := cutOutermost(s, '{', '}')
	if !ok {
		return extractOutput{}, false
	}
	var out extractOutput
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return extractOutput{}, false
	}
	return out, out.Memories != nil || out.Mentioned != nil
}

// parseArrayForm 解析直接给出记忆数组的简化形式。
func parseArrayForm(s string) (extractOutput, bool) {
	body, ok := cutOutermost(s, '[', ']')
	if !ok {
		return extractOutput{}, false
	}
	var items []extracted
	if err := json.Unmarshal([]byte(body), &items); err != nil {
		return extractOutput{}, false
	}
	return extractOutput{Memories: items}, true
}

// cutOutermost 截出最外层的一对括号；找不到时返回 false。
func cutOutermost(s string, open, close byte) (string, bool) {
	start := strings.IndexByte(s, open)
	end := strings.LastIndexByte(s, close)
	if start < 0 || end <= start {
		return "", false
	}
	return s[start : end+1], true
}

// serializeHistory 把历史压成便于提炼的纯文本。工具结果与思考过程一律截断——
// 提炼要的是结论，长篇的中间过程只会稀释信号并推高这次调用的成本。
func serializeHistory(lang string, msgs []llm.Message, maxBytes int) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			b.WriteString(prompt.Tf(lang, "prompt.memory.dialogue.user", "用户: %s\n", m.Content))
		case llm.RoleAssistant:
			if m.Content != "" {
				b.WriteString(prompt.Tf(lang, "prompt.memory.dialogue.assistant", "助手: %s\n", m.Content))
			}
			for _, tc := range m.ToolCalls {
				b.WriteString(prompt.Tf(lang, "prompt.memory.dialogue.tool_call", "[调用工具 %s %s]\n",
					tc.Name, truncateRunes(string(tc.Arguments), 160)))
			}
		case llm.RoleTool:
			b.WriteString(prompt.Tf(lang, "prompt.memory.dialogue.tool_result", "[工具结果] %s\n",
				truncateRunes(m.Content, 300)))
		}
	}
	return clampDialogue(lang, b.String(), maxBytes)
}

// clampDialogue 给送去提炼的对话文本收口。
func clampDialogue(lang, s string, maxBytes int) string {
	out, truncated := plugin.TruncateUTF8([]byte(s), maxBytes)
	res := string(out)
	if truncated {
		res += prompt.T(lang, "prompt.memory.dialogue.truncated", "\n...[对话过长已截断]")
	}
	return res
}
