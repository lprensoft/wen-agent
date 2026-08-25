package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"wen/internal/pathsafe"
)

// bom 是有些编辑器会写在文件开头的字节序标记 U+FEFF，它不属于内容。
const bom = string(rune(0xFEFF))

// skillFile 是一个技能目录里必须有的那个文件。
const skillFile = "SKILL.md"

// validSkillName 限定技能名的取值。技能名来自目录名，同时也是 read_skill 的参数，
// 因此按路径片段的标准约束。允许连字符——外面拿来的技能包常这么起名；
// 排除点开头，免得把 .git、.obsidian 之类的目录当成技能。
var validSkillName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// skill 是一份已加载的技能手册。
type skill struct {
	Name string // 目录名，技能的唯一标识
	Desc string // frontmatter 里的 description，已截断
	Dir  string // 该技能自己的目录，手册里提到的随附文件都在这儿
	File string // SKILL.md 的完整路径
}

// scanResult 是一次扫描的产出。problems 是给人看的（设置页与状态行），
// 不进模型上下文：模型对「哪个文件写坏了」无能为力，那是用户要修的东西。
type scanResult struct {
	skills   []skill
	problems []string
	missing  bool // 目录不存在
}

// scan 扫描技能目录。descLimit 是单条用途的字数上限。
//
// 一处坏掉不影响其余：装了十个技能，其中一个的 frontmatter 写错了，
// 该加载的九个照样要加载，坏的那个单独报出来。
func scan(dir string, descLimit int) scanResult {
	var res scanResult
	if dir == "" {
		return res
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			res.missing = true
			res.problems = append(res.problems, "技能目录不存在："+dir)
		} else {
			res.problems = append(res.problems, "读取技能目录失败："+err.Error())
		}
		return res
	}
	// os.ReadDir 按文件名排序，清单因此是稳定的——SystemPrompt 的返回值要求逐字节一致
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // 版本控制与编辑器的目录，不是技能，也不该报成问题
		}
		sdir := filepath.Join(dir, name)
		// 用 Stat 判断而不是 e.IsDir()：后者的类型来自 lstat，指向目录的符号链接与
		// Windows 的目录联接在它眼里都不是目录。只判 e.IsDir 的话，链接进来的技能会
		// 在这一步被悄悄丢掉——既加载不上，也没有一句话说明为什么。
		if info, err := os.Stat(sdir); err != nil || !info.IsDir() {
			continue
		}
		if !validSkillName.MatchString(name) {
			res.problems = append(res.problems,
				fmt.Sprintf("%s：目录名只能用小写字母、数字、下划线与连字符，已跳过", name))
			continue
		}
		// 技能目录不得经链接指到技能根之外。指到根**之内**的链接照常收下——那是用户
		// 在自己目录里做的整理，没有理由拦。
		//
		// 拒绝时必须说一句：一个技能没出现在清单里而屏幕上什么都没有，是最难自己查
		// 出来的一类问题。插件包是从外面拿来的，Agent Plugins 标准也明确要求客户端
		// 拒绝这种逃逸。
		if !pathsafe.Within(dir, sdir) {
			res.problems = append(res.problems, name+"：经链接指到了技能目录之外，已跳过")
			continue
		}
		path := filepath.Join(sdir, skillFile)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				res.problems = append(res.problems, name+"：目录下没有 "+skillFile+"，已跳过")
			} else {
				res.problems = append(res.problems, name+"：读取 "+skillFile+" 失败——"+err.Error())
			}
			continue
		}
		meta, err := parseMeta(string(data))
		if err != nil {
			res.problems = append(res.problems, name+"："+err.Error()+"，已跳过")
			continue
		}
		if meta.Name != "" && meta.Name != name {
			// 不以 frontmatter 的名字为准：标识必须与目录一一对应，否则重名或改名
			// 之后 read_skill 拿到的名字就找不到对应的目录了。只提醒一句。
			res.problems = append(res.problems,
				fmt.Sprintf("%s：文件里写的名称是 %q，与目录名不一致，已按目录名加载", name, meta.Name))
		}
		res.skills = append(res.skills, skill{
			Name: name,
			Desc: truncRunes(meta.Description, descLimit),
			Dir:  sdir,
			File: path,
		})
	}
	return res
}

// meta 是 SKILL.md 开头 frontmatter 里我们关心的字段。
type meta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// parseMeta 取出 frontmatter。没有 description 视为解析失败：
// 清单里那一行用途就是模型判断「该不该读这份手册」的全部依据，缺了它这个技能
// 永远不会被用上，静默收录只会让人以为装好了。
func parseMeta(src string) (meta, error) {
	fm, _, ok := splitFrontMatter(src)
	if !ok {
		return meta{}, fmt.Errorf("开头缺少 --- 包起来的说明块")
	}
	var m meta
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		return meta{}, fmt.Errorf("开头的说明块解析失败——%s", err.Error())
	}
	m.Name = strings.TrimSpace(m.Name)
	m.Description = strings.Join(strings.Fields(m.Description), " ")
	if m.Description == "" {
		return meta{}, fmt.Errorf("开头的说明块里没有写 description")
	}
	return m, nil
}

// splitFrontMatter 切开 --- 包起来的开头说明块与正文。
// 结束标记要求整行只有 ---，否则正文里一行 ---- 分隔线就会被当成结束。
func splitFrontMatter(src string) (fm, body string, ok bool) {
	s := strings.ReplaceAll(strings.TrimPrefix(src, bom), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return "", s, false
	}
	lines := strings.Split(s, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"),
				strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\n"), true
		}
	}
	return "", s, false
}

// truncRunes 按字符数截断，不切坏多字节字符。
func truncRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
