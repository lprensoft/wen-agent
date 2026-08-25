// Package skills 提供「技能手册」插件：把用户自己安装的 SKILL.md 交给模型按需取用。
//
// 它不提供任何新能力——技能只是文字，讲的是「这类事该怎么做」，用的还是别的插件
// 已经给出的工具。之所以做成插件而不是配置里的一段提示词，是因为技能有两个特点：
// 数量会长，且大多数时候与手头的事无关。全部塞进系统提示词等于每一轮都为用不上的
// 内容付钱，所以这里只常驻一份「名称 + 用途」的清单，正文由模型用 read_skill 按需取。
//
// 正文走工具返回值而不是任何提示词注入，是这套上下文分层里唯一站得住的落点：工具
// 结果追加进历史，此后前缀不再变动，提示词缓存照常命中；而按需插进系统提示词或
// 每轮状态块的内容，每次变动都会把整段前缀作废。
package skills

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"wen/internal/plugin"
	"wen/internal/skillroots"
)

const (
	defaultMaxList  = 30
	defaultMaxBytes = 32 * 1024
	// maxDescRunes 是清单里单条用途的长度上限。清单每轮重发，条数与单条长度都得有上限，
	// 否则一份写得啰嗦的技能就能把整个清单撑成上下文里最大的一块。
	maxDescRunes = 120
)

// Plugin 是 skills 插件。
type Plugin struct {
	// mu 保护下面这一组：Init 会在运行时被重复调用（改配置即重新 Init），
	// 而那时可能有 in-flight 的 Execute 正在读它们。
	mu       sync.RWMutex
	dir      string
	maxList  int
	maxBytes int
	list     []skill
	index    map[string]skill
	problems []string
	prompt   string // 已渲染好的清单，SystemPrompt 直接返回，不在调用时做任何 I/O

	// scanMu 串行化整轮扫描。扫描要读一批目录，不能在 mu 里做（那会把清单锁住
	// 一整趟 I/O）；而两趟并发的扫描若各扫各的再写回，晚到的旧结果会盖掉新的。
	scanMu sync.Mutex
	// unsub 取消对技能根登记的订阅。Init 可能被反复调用，只订阅一次。
	unsub func()

	// 设置页操作的状态，与主状态分开加锁：StartAction 与 ActionState 由 HTTP 并发调用。
	actMu    sync.Mutex
	actSeq   uint64
	actState plugin.ActionState
}

func New() *Plugin {
	return &Plugin{maxList: defaultMaxList, maxBytes: defaultMaxBytes}
}

func (p *Plugin) Name() string     { return "skills" }
func (p *Plugin) Category() string { return plugin.CategoryTools }

func (p *Plugin) Description() string {
	return "技能手册：把目录下的 SKILL.md 列成清单，模型按需读取其中的步骤"
}

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key:         "dir",
			Label:       "技能目录",
			Type:        plugin.FieldString,
			Description: "存放技能的目录，每个技能是其中的一个子目录。留空则用默认目录（配置目录下的 plugins/skills），点下面的扫描按钮可以看到它的完整路径。",
			Default:     "",
		},
		{
			Key:         "max_list",
			Label:       "清单最多列出",
			Type:        plugin.FieldInt,
			Description: "常驻清单里最多列出多少个技能。超出的部分不会消失，模型仍可自行查看完整清单。",
			Default:     defaultMaxList,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(200),
		},
		{
			Key:         "max_bytes",
			Label:       "单个技能最大读取字节数",
			Type:        plugin.FieldInt,
			Description: "读取一个技能正文时返回的最大字节数，超出部分截断。",
			Default:     defaultMaxBytes,
			Min:         plugin.IntPtr(1024),
			Max:         plugin.IntPtr(512 * 1024),
		},
	}
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	dir := strings.TrimSpace(plugin.CfgString(cfg, "dir", ""))
	own := dir == ""
	if own {
		// 没有可用的持久化位置时拒绝启用，不退化到写进程当前目录
		if ictx.StateDir == "" {
			return fmt.Errorf("没有可用的技能目录，请在配置里指定一个")
		}
		dir = ictx.StateDir
		// 默认目录归本插件所有，先建出来，用户才知道该往哪放文件
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建技能目录失败: %w", err)
		}
	}
	// 用户自己指定的目录不代建：路径打错时「目录不存在」是有用的反馈，
	// 而默默建一个空目录只会让人对着它纳闷技能为什么没加载。

	p.mu.Lock()
	p.dir = dir
	p.maxList = plugin.CfgInt(cfg, "max_list", defaultMaxList)
	p.maxBytes = plugin.CfgInt(cfg, "max_bytes", defaultMaxBytes)
	if p.unsub == nil {
		// 外源插件包在启用时登记自己的技能目录，而它可能排在本插件之后注册，
		// 用户也可能在设置页上随时开关一个包——只在这里扫一次的话，那些技能
		// 要重启才出现。
		p.unsub = skillroots.Subscribe(p.rescan)
	}
	p.mu.Unlock()

	p.rescan()
	return nil
}

// Stop 取消订阅。插件被禁用或重配时由 Manager 在锁外调用。
func (p *Plugin) Stop() {
	p.mu.Lock()
	unsub := p.unsub
	p.unsub = nil
	p.mu.Unlock()
	if unsub != nil {
		unsub()
	}
}

// rescan 扫一遍全部技能根：用户自己的目录，加上外源插件包登记进来的那些。
//
// 用户的目录排在最前，重名时它赢：那是用户直接控制的东西，装进来的包不该悄悄
// 盖掉它。同名的后来者跳过并报一条，而不是两条都收——read_skill 按名字找，
// 收下两个同名的等于让它随机命中一个。
func (p *Plugin) rescan() {
	p.scanMu.Lock()
	defer p.scanMu.Unlock()

	p.mu.RLock()
	dir, maxList := p.dir, p.maxList
	p.mu.RUnlock()

	var merged scanResult
	seen := map[string]string{} // 技能名 → 来源说明

	take := func(label string, res scanResult) {
		for _, s := range res.skills {
			if from, dup := seen[s.Name]; dup {
				merged.problems = append(merged.problems,
					fmt.Sprintf("%s：与%s里的同名技能重复，已跳过", s.Name, from))
				continue
			}
			seen[s.Name] = label
			merged.skills = append(merged.skills, s)
		}
		for _, pr := range res.problems {
			if label != "" {
				pr = label + " " + pr
			}
			merged.problems = append(merged.problems, pr)
		}
	}

	own := scan(dir, maxDescRunes)
	merged.missing = own.missing
	take("技能目录", own)
	for _, r := range skillroots.List() {
		take("插件包 "+r.Label, scan(r.Dir, maxDescRunes))
	}

	p.mu.Lock()
	p.apply(merged, maxList)
	p.mu.Unlock()
}

// apply 写入一次扫描结果并重新渲染清单。调用者须持有写锁。
func (p *Plugin) apply(res scanResult, maxList int) {
	p.list = res.skills
	p.problems = res.problems
	p.index = make(map[string]skill, len(res.skills))
	for _, s := range res.skills {
		p.index[s.Name] = s
	}
	p.prompt = renderPrompt(res.skills, maxList)
}

// SystemPrompt 返回常驻清单。它只读一份渲染好的快照——按契约必须廉价无副作用，
// 且返回值要在轮次之间逐字节一致，否则整段前缀的提示词缓存永远命中不了。
func (p *Plugin) SystemPrompt() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.prompt
}

func (p *Plugin) StatusLines(lang string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.dir == "" {
		return nil
	}
	line := fmt.Sprintf("📚 技能：已加载 %d 个", len(p.list))
	if n := len(skillroots.List()); n > 0 {
		line += fmt.Sprintf("（含 %d 个插件包）", n)
	}
	if n := len(p.problems); n > 0 {
		line += fmt.Sprintf("，%d 个未能加载", n)
	}
	return []string{line + "（" + p.dir + "）"}
}

// renderPrompt 渲染常驻清单；一个技能都没有时返回空串，不注入任何东西。
func renderPrompt(list []skill, maxList int) string {
	if len(list) == 0 {
		return ""
	}
	shown := list
	if maxList > 0 && len(shown) > maxList {
		shown = shown[:maxList]
	}
	var b strings.Builder
	b.WriteString("[可用技能]\n")
	b.WriteString("以下是已安装的技能手册，每条只给出名称与用途。着手一件事之前先扫一眼这份清单：" +
		"与某条用途相符时，用 read_skill 读出它的完整步骤再动手，不要凭名称猜里面写了什么；" +
		"都不相符就照常处理，不必读取。\n")
	for _, s := range shown {
		fmt.Fprintf(&b, "- %s：%s\n", s.Name, s.Desc)
	}
	// 超限时降级要保住「还有东西存在」这个信息，而不是让它们静默消失
	if n := len(list) - len(shown); n > 0 {
		fmt.Fprintf(&b, "另有 %d 个技能未在此列出，用 list_skills 可看到完整清单。\n", n)
	}
	return strings.TrimRight(b.String(), "\n")
}
