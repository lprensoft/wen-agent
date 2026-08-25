package plugin

// 本文件是插件功能分组的公共约定。分组只影响设置页的展示：同组的插件在界面上
// 聚成一节，组内与组间的先后仍由注册顺序决定（它承载提示词注入顺序与依赖关系，
// 不为展示而改动）。

// 功能分组的取值。分组名直接作为界面上的小节标题展示。
const (
	CategoryTools      = "基础工具"
	CategoryMemory     = "记忆与检索"
	CategoryPersona    = "角色演绎"
	CategoryBackground = "后台任务"
	CategoryChannel    = "消息通道"
	CategoryProgram    = "程序维护"
	CategoryExternal   = "外源插件"
	CategoryOther      = "其他"
)

// Categorized 是插件的可选能力：声明所属的功能分组。
// 未实现该接口（或返回空串）的插件归入「其他」——外部来源的插件不声明也能正常展示。
type Categorized interface {
	Category() string
}

// CategoryOf 返回插件声明的分组；未声明时归入「其他」。
func CategoryOf(p Plugin) string {
	if c, ok := p.(Categorized); ok {
		if cat := c.Category(); cat != "" {
			return cat
		}
	}
	return CategoryOther
}

// 分组的稳定标识。分组名本身是中文，翻译之后就不能再拿它当分组的身份——
// 界面按标识分组、按译名展示，否则英文界面下同一组会因为译名不同而裂成两组，
// 前端那句 `p.category || "其他"` 的兜底也会凭空造出一个「Other」组。
const (
	CategoryKeyTools      = "tools"
	CategoryKeyMemory     = "memory"
	CategoryKeyPersona    = "persona"
	CategoryKeyBackground = "background"
	CategoryKeyChannel    = "channel"
	CategoryKeyProgram    = "program"
	CategoryKeyExternal   = "external"
	CategoryKeyOther      = "other"
)

var categoryKeys = map[string]string{
	CategoryTools:      CategoryKeyTools,
	CategoryMemory:     CategoryKeyMemory,
	CategoryPersona:    CategoryKeyPersona,
	CategoryBackground: CategoryKeyBackground,
	CategoryChannel:    CategoryKeyChannel,
	CategoryProgram:    CategoryKeyProgram,
	CategoryExternal:   CategoryKeyExternal,
	CategoryOther:      CategoryKeyOther,
}

// CategoryKeyOf 返回分组的稳定标识；外部插件自定的分组名没有标识，归「其他」
// ——它的展示名仍是插件自己给的那个字，只是不参与翻译。
func CategoryKeyOf(cat string) string {
	if k, ok := categoryKeys[cat]; ok {
		return k
	}
	return CategoryKeyOther
}
