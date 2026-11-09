package modelcfg

import (
	"reflect"
	"slices"
	"strings"
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
func carryOverKeys(next, old File) File {
	out := cloneFile(next)
	for i, p := range out.Providers {
		if strings.TrimSpace(p.APIKey) != "" {
			continue
		}
		if prev, ok := findProvider(old.Providers, p.Name); ok {
			out.Providers[i].APIKey = prev.APIKey
		}
	}
	return out
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

// normalize 规整用户输入：去空白、去 base_url 末尾斜杠。
func normalize(f *File) {
	for i := range f.Providers {
		p := &f.Providers[i]
		p.Name = strings.TrimSpace(p.Name)
		p.Type = strings.TrimSpace(p.Type)
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		p.APIKey = strings.TrimSpace(p.APIKey)
		p.Dialect = strings.TrimSpace(p.Dialect)
		p.Source = ""
		for j := range p.Models {
			m := &p.Models[j]
			m.ID = strings.TrimSpace(m.ID)
			m.Name = strings.TrimSpace(m.Name)
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
