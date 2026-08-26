// Package bodysense 提供身体感知的系统插件：记录角色身体各部位被触碰的累计次数，
// 按累计次数划分熟悉阶段，并把状态与「不同阶段该有什么反应」的准则注入上下文，
// 使同一处接触在第一次与第二十次有不同的反应。
//
// 插件只提供机制与状态，不写台词——具体怎么演由模型按角色设定决定。
//
// 本插件依赖 roleplay：没有角色，身体就没有归属。
package bodysense

import (
	"fmt"
	"strings"

	"wen/internal/prompt"
)

// 私密度分档。它不参与熟悉度的计算，只随状态注入，由反应准则消费：
// 私密度决定「起点多强烈」与「衰减到什么程度为止」，熟悉度决定「衰减了多少」。
// 若把两者相乘成一个阈值，等于用一个维度表达两件事，两边都表达不好。
const (
	privacyDaily    = 0
	privacyClose    = 1
	privacyIntimate = 2
	privacyPrivate  = 3
)

const (
	// maxParts / maxPartsBytes 限制部位表的体量：部位名随工具声明每轮发送。
	// 超出报错而不是截断——被截掉的部位模型看不见，用户却以为配上了。
	maxParts      = 60
	maxPartsBytes = 4 * 1024
	// maxPartRunes 按字数算，得容得下拉丁文字：中文的「后颈」两个字，英文的
	// "back of the neck" 是十六个。限制真正防的是体量，那由 maxParts 与
	// maxPartsBytes 兜着，这一条只挡「把一句话填成部位名」。
	maxPartRunes = 24
)

// 内置默认部位表。分档按「在什么关系阶段才会自然发生」划，不按解剖学。
//
// 几处有意的取舍：「手」与「手指/手背/手腕」分开——牵手、碰指尖、握手腕是三种不同
// 强度的接触，合并会让最常用的部位失去分辨率；「腰」放在亲密档而不是亲近档，中文
// 语境里搭腰是明确的亲密动作；不分左右，分了计数摊薄一半而叙事上从不区分；私密档
// 只留三条，条目越多模型越倾向往这一档上报，而这一档误报的代价最高。
const (
	defaultPartsDaily    = "手\n手背\n手指\n手腕\n肩\n上臂\n后背\n头发"
	defaultPartsClose    = "脸颊\n下巴\n耳朵\n后颈\n脖颈\n锁骨\n头顶\n膝盖\n小腿\n脚踝"
	defaultPartsIntimate = "嘴唇\n耳垂\n腰\n后腰\n腹部\n大腿"
	defaultPartsPrivate  = "胸口\n臀\n大腿内侧"
)

// Part 是部位表里的一条：名称与私密度。
type Part struct {
	Name    string
	Privacy int
}

// 私密度的四个说法，用于注入与工具回显。
var (
	privacyDailyText    = prompt.Text("prompt.body_sense.privacy.daily", "日常")
	privacyCloseText    = prompt.Text("prompt.body_sense.privacy.close", "亲近")
	privacyIntimateText = prompt.Text("prompt.body_sense.privacy.intimate", "亲密")
	privacyPrivateText  = prompt.Text("prompt.body_sense.privacy.private", "私密")
)

// privacyLabel 给出私密度的说法。
func privacyLabel(lang string, p int) string {
	switch p {
	case privacyDaily:
		return privacyDailyText.For(lang)
	case privacyClose:
		return privacyCloseText.For(lang)
	case privacyIntimate:
		return privacyIntimateText.For(lang)
	default:
		return privacyPrivateText.For(lang)
	}
}

// parseParts 把四档配置文本解析成部位表。四档合一后保持「日常 → 私密」的顺序，
// 注入与 enum 都按这个顺序，读起来是一条从外到内的梯度。
//
// 校验失败一律返回 error 而不是跳过：Manager.SetConfig 会把 Init 的错误原样回给
// 设置页并回滚旧配置，这是用户看得见的通道；只记日志的话用户会以为那一行生效了。
func parseParts(lang, daily, closeUp, intimate, private string) ([]Part, error) {
	if n := len(daily) + len(closeUp) + len(intimate) + len(private); n > maxPartsBytes {
		return nil, fmt.Errorf(prompt.T(lang, "prompt.body_sense.parts.too_long",
			"部位表过长（%d 字节，上限 %d 字节），请精简后重试"), n, maxPartsBytes)
	}

	groups := []struct {
		text    string
		privacy int
		label   prompt.Piece
	}{
		{daily, privacyDaily, prompt.Text("prompt.body_sense.tier.daily", "日常可触部位")},
		{closeUp, privacyClose, prompt.Text("prompt.body_sense.tier.close", "需要亲近才会触碰的部位")},
		{intimate, privacyIntimate, prompt.Text("prompt.body_sense.tier.intimate", "亲密部位")},
		{private, privacyPrivate, prompt.Text("prompt.body_sense.tier.private", "私密部位")},
	}

	var out []Part
	seen := map[string]string{} // 规范名 → 已出现的档位名
	for _, g := range groups {
		for i, raw := range strings.Split(g.text, "\n") {
			name := strings.Join(strings.Fields(raw), "")
			if name == "" {
				continue
			}
			if n := len([]rune(name)); n > maxPartRunes {
				return nil, fmt.Errorf(prompt.T(lang, "prompt.body_sense.parts.name_long",
					"「%s」第 %d 行的部位名过长（%d 字，上限 %d 字）"),
					g.label.For(lang), i+1, n, maxPartRunes)
			}
			key := strings.ToLower(name)
			if prev, dup := seen[key]; dup {
				return nil, fmt.Errorf(prompt.T(lang, "prompt.body_sense.parts.duplicate",
					"部位 %q 重复出现（%s 与 %s），同一部位只能属于一个档位"),
					name, prev, g.label.For(lang))
			}
			seen[key] = g.label.For(lang)
			out = append(out, Part{Name: name, Privacy: g.privacy})
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%s", prompt.T(lang, "prompt.body_sense.parts.all_empty",
			"四档部位表全为空，至少要有一个部位"))
	}
	if len(out) > maxParts {
		return nil, fmt.Errorf(prompt.T(lang, "prompt.body_sense.parts.too_many",
			"部位共 %d 个，超出上限 %d 个，请精简后重试"), len(out), maxParts)
	}
	return out, nil
}

// findPart 按名称查找部位（大小写与空白不敏感）。
func findPart(parts []Part, name string) (Part, bool) {
	want := strings.ToLower(strings.Join(strings.Fields(name), ""))
	for _, p := range parts {
		if strings.ToLower(p.Name) == want {
			return p, true
		}
	}
	return Part{}, false
}

// partNames 按表内顺序列出全部部位名，用于工具 schema 的 enum 与不匹配时的报错。
// 列出的是配置表里的部位名，与可见域无关——改成「列出已记录的部位」会让不可读域的
// 存在性从报错文案里漏出去。
func partNames(parts []Part) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Name)
	}
	return out
}
