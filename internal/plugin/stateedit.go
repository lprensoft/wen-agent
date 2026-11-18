package plugin

// 状态页的写入通道。
//
// StateReporter 的契约是只读的（不写盘、不调模型、只给人看）；这里另给一个可选的
// StateEditor，让插件声明「这一行/这一块能改什么、怎么改」，核心只负责把请求送回
// 插件。核心仍然不知道「人物」「记忆」「日记」这些概念，也不解释 Op 的语义。
//
// 与 Actionable 的分工：那是插件级、全局、带轮询的长流程；这里是行级、同步、只做
// 一次落盘的状态修正。两者都遵循同一条：按钮的含义取决于展示时的内容，插件必须把
// 客户端带回来的身份当不可信输入核对。
//
// 实现要点：
//   - State() 里挂上 Edits / ID 描述可改项；它们必须在 pinned 查询里被裁掉。
//   - EditState 自行加锁，并按可见域找库；Domain 来自客户端，只认磁盘上真有的域，
//     绝不拿它拼路径。
//   - 错误文案是给人看的，走 i18n.T(req.Lang, ...)，不进模型上下文。
type StateEditor interface {
	EditState(req StateEditRequest) error
}

// StateEditRequest 是一次状态修正请求。
//
// Section / Row / Op 都来自客户端：插件按自己的数据核对它们指向的是不是真的存在，
// 不能当路径或索引直接使用。Row 为空表示这是块级操作（新增、整块重置）。
type StateEditRequest struct {
	Lang    string
	Domain  string
	Section string
	Row     string
	Op      string
	Values  map[string]string
}

// StateEdit 是状态页上可触发的一次修改。
type StateEdit struct {
	// Op 是插件内的操作标识，静态、不翻译。
	Op string `json:"op"`
	// Label 是按钮文字，静态、已按请求语言译好。
	Label string `json:"label"`
	// Fields 为空表示点了即执行（删除、重置、批准）；非空时界面先弹表单。
	Fields []StateField `json:"fields,omitempty"`
	// Confirm 非空时界面先让用户确认；由插件写清后果，静态、已译。
	Confirm string `json:"confirm,omitempty"`
	// Danger 只影响按钮颜色，不承载语义。
	Danger bool `json:"danger,omitempty"`
}

// StateField 是编辑表单里的一个字段。
//
// Value 是编辑时的现值；新增表单里由插件给空值或默认值。Type 只允许下面四种，
// 界面按它渲染控件；值一律以字符串往返，插件自己解析与校验。
type StateField struct {
	Key      string        `json:"key"`
	Label    string        `json:"label"`
	Type     string        `json:"type"`
	Value    string        `json:"value,omitempty"`
	Options  []StateOption `json:"options,omitempty"`
	MaxLen   int           `json:"max_len,omitempty"`
	Min      *int          `json:"min,omitempty"`
	Max      *int          `json:"max,omitempty"`
	Required bool          `json:"required,omitempty"`
}

// StateOption 是 select 字段的一个候选。Value 是落盘规范值（可能是中文枚举），
// Label 已按请求语言译好。
type StateOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// 状态编辑字段的类型。界面只认这四种；未知类型按单行文本渲染。
const (
	StateFieldText     = "text"
	StateFieldTextarea = "textarea"
	StateFieldNumber   = "number"
	StateFieldSelect   = "select"
)

const (
	// maxStateEdits 是单行/单块最多几个操作。
	maxStateEdits = 4
	// maxStateFields 是单个操作最多几个字段。
	maxStateFields = 8
	// MaxStateEditValues 是一次请求最多带几个值，HTTP 层据此限幅。
	MaxStateEditValues = 16
	// MaxStateEditBodyBytes 是请求体上限，在 HTTP 层执行。
	MaxStateEditBodyBytes = 64 << 10
)

// clampEdits 给插件返回的编辑描述兜一层硬上限。不改插件自己的切片：只在超限时
// 复制一份，正常路径下仍返回原切片——状态页不是热路径，但也没必要每次多份分配。
func clampEdits(in []StateEdit) []StateEdit {
	if len(in) == 0 {
		return nil
	}
	over := len(in) > maxStateEdits
	if !over {
		for _, e := range in {
			if len(e.Fields) > maxStateFields {
				over = true
				break
			}
		}
	}
	if !over {
		return in
	}
	out := make([]StateEdit, 0, min(len(in), maxStateEdits))
	for _, e := range in[:min(len(in), maxStateEdits)] {
		if len(e.Fields) > maxStateFields {
			e.Fields = append([]StateField(nil), e.Fields[:maxStateFields]...)
		}
		out = append(out, e)
	}
	return out
}

// stateEditSize 把编辑描述计入状态块的字节上限。字段值可能与行里已有的文本重复，
// 不计的话，插件可以借编辑表单绕开每插件 48KB 的兜底。
func stateEditSize(e StateEdit) int {
	n := len(e.Op) + len(e.Label) + len(e.Confirm)
	for _, f := range e.Fields {
		n += len(f.Key) + len(f.Label) + len(f.Type) + len(f.Value)
		for _, o := range f.Options {
			n += len(o.Value) + len(o.Label)
		}
	}
	return n
}
