package modelcfg

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"wen/internal/i18n"
)

// mergeWithBase 把 models.json（own）与 config.yaml 的提供商列表合并：
//   - 同名条目：models.json 完全覆盖（界面上的改动优先）
//   - 只在 models.json：保留，顺序按数组
//   - 只在 config.yaml 且未被删除：追加到末尾，标记 source=config
//   - 只在 config.yaml 且已被删除（坠碑）：不出现
func mergeWithBase(own File, base []Provider, baseSel Selection) File {
	out := File{Version: 1, Current: own.Current, Removed: append([]string(nil), own.Removed...),
		Fallbacks: append([]Selection(nil), own.Fallbacks...)}

	for _, p := range own.Providers {
		p.Source = ""
		p.Models = append([]Model(nil), p.Models...)
		out.Providers = append(out.Providers, p)
	}
	for _, b := range base {
		if _, taken := findProvider(out.Providers, b.Name); taken {
			continue
		}
		if containsFold(own.Removed, b.Name) {
			continue
		}
		b.Source = "config"
		b.Models = append([]Model(nil), b.Models...)
		out.Providers = append(out.Providers, b)
	}

	out.Current = pickCurrent(out.Providers, own.Current, baseSel)
	return out
}

// pickCurrent 依次尝试：models.json 记录的选择 → config.yaml 的 model 段 → 第一个可用组合。
func pickCurrent(ps []Provider, own, base Selection) Selection {
	for _, sel := range []Selection{own, base} {
		if p, ok := findProvider(ps, sel.Provider); ok {
			if _, ok := findModel(p.Models, sel.Model); ok {
				return Selection{Provider: p.Name, Model: sel.Model}
			}
		}
	}
	for _, p := range ps {
		if len(p.Models) > 0 {
			return Selection{Provider: p.Name, Model: p.Models[0].ID}
		}
	}
	return Selection{}
}

// carryOverKeys 把请求中留空的 api_key 用旧值补上（界面只拿得到掩码）。
//
// 匹配先看界面带回的原始标识 Ref（见 Provider.Ref）：旧版本允许只差大小写的两个
// 提供商共存，按名字找会同时命中两家——改名保存时第一家会被清空、第二家反而被写入
// 第一家的密钥，凭据就可能发往另一家的地址。Ref 跟着条目走，改名也不丢。
//
// 没带 Ref 的客户端退回精确名字匹配，不再用 EqualFold：那正是把密钥带到另一家的
// 原因。匹配不到（新条目）就留空，让用户自己填——宁可多要一次密钥，也不把凭据
// 抄给另一家。
//
// 带了 Ref 却对不上旧配置里任何一条时，整次保存拒绝，而不是把空密钥写下去：这份
// 提交是在别的改动之前取的（首次保存成功但响应丢了之后的重试、另一个窗口改过名字），
// 回填不上还照写，等于替用户把密钥清了。
func carryOverKeys(lang string, next, old File) (File, error) {
	out := cloneFile(next)
	byRef := make(map[string]Provider, len(old.Providers))
	for i, p := range old.Providers {
		byRef[providerRef(old.Providers, i)] = p
	}
	byName := make(map[string]Provider, len(old.Providers))
	for _, p := range old.Providers {
		if _, ok := byName[p.Name]; !ok {
			byName[p.Name] = p
		}
	}
	for i, p := range out.Providers {
		if p.Ref == "" {
			continue
		}
		prev, ok := byRef[p.Ref]
		if !ok {
			return File{}, fmt.Errorf(i18n.T(lang, "modelcfg.err.stale_ref",
				"提供商 %q 与当前配置对不上：配置已在别处被修改，请刷新后重试"), p.Name)
		}
		if strings.TrimSpace(p.APIKey) == "" {
			out.Providers[i].APIKey = prev.APIKey
		}
	}
	for i, p := range out.Providers {
		if p.Ref != "" || strings.TrimSpace(p.APIKey) != "" {
			continue
		}
		if prev, ok := byName[p.Name]; ok {
			out.Providers[i].APIKey = prev.APIKey
		}
	}
	return out, nil
}

// tombstones 重算删除坠碑：config.yaml 里存在但本次提交中没有的提供商记为已删除，
// 重新出现的则从坠碑列表中移除。
func tombstones(next []Provider, base []Provider, old []string) []string {
	var out []string
	for _, b := range base {
		if _, ok := findProvider(next, b.Name); !ok {
			out = append(out, b.Name)
		}
	}
	// 保留那些指向已不存在于 config.yaml 的旧坠碑，避免配置文件临时改动丢状态
	for _, name := range old {
		if _, inBase := findProvider(base, name); inBase {
			continue
		}
		if _, inNext := findProvider(next, name); inNext {
			continue
		}
		if !containsFold(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// dropUnchangedBase 丢弃与 config.yaml 完全一致、且此前也没被 models.json 接管的条目，
// 让它们继续跟随配置文件（避免任何一次保存就把全部提供商固化下来）。
func dropUnchangedBase(next, base, prevOwn []Provider) []Provider {
	var out []Provider
	for _, p := range next {
		b, inBase := findProvider(base, p.Name)
		_, owned := findProvider(prevOwn, p.Name)
		if inBase && !owned && sameProvider(p, b) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func sameProvider(a, b Provider) bool {
	a.Source, b.Source = "", ""
	if len(a.Models) == 0 {
		a.Models = nil
	}
	if len(b.Models) == 0 {
		b.Models = nil
	}
	return reflect.DeepEqual(a, b)
}

// normalize 规整用户输入：去空白、去 base_url 末尾斜杠、币种统一大写。
func normalize(f *File) {
	for i := range f.Providers {
		p := &f.Providers[i]
		p.Name = strings.TrimSpace(p.Name)
		p.Type = strings.TrimSpace(p.Type)
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		p.APIKey = strings.TrimSpace(p.APIKey)
		p.Dialect = strings.TrimSpace(p.Dialect)
		p.Protocol = strings.TrimSpace(p.Protocol)
		p.Source = ""
		p.Ref = "" // 界面往返用的原始标识：只服务 carryOverKeys，不落盘
		for j := range p.Models {
			m := &p.Models[j]
			m.ID = strings.TrimSpace(m.ID)
			m.Name = strings.TrimSpace(m.Name)
			// 币种是金额的分桶键（见 usage.CostTotals）：落盘前统一成大写去空白，
			// 手填的 " usd " 才会与预算里的 "USD" 落进同一桶，界面上的徽标也不必
			// 跟着用户敲的大小写变来变去。
			if m.Price != nil {
				m.Price.Currency = strings.ToUpper(strings.TrimSpace(m.Price.Currency))
			}
			if m.Thinking != nil {
				t := strings.TrimSpace(*m.Thinking)
				if t == "" {
					m.Thinking = nil
				} else {
					m.Thinking = &t
				}
			}
		}
	}
	f.Current.Provider = strings.TrimSpace(f.Current.Provider)
	f.Current.Model = strings.TrimSpace(f.Current.Model)

	out := f.Fallbacks[:0]
	for _, sel := range f.Fallbacks {
		sel.Provider = strings.TrimSpace(sel.Provider)
		sel.Model = strings.TrimSpace(sel.Model)
		if sel.Provider == "" && sel.Model == "" {
			continue
		}
		// 主模型自己不必再当备用模型；切换主模型时 SetCurrent 也会清掉对应项。
		if strings.EqualFold(sel.Provider, f.Current.Provider) && sel.Model == f.Current.Model {
			continue
		}
		out = append(out, sel)
	}
	f.Fallbacks = out
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(v string) bool { return strings.EqualFold(v, s) })
}
