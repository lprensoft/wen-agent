package plugin

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// 本文件是「按可见域分目录」的公共约定。memory 与 session_search 都要把自己的数据
// 按可见域分开存放，两边各写一套迟早会漂移，因此放在这里共用。

// DomainDir 返回某个可见域对应的数据目录：空标签用基准目录本身，带标签的用同级的
// <base>-<tag>。基准目录保持原样，使升级前已有的数据不需要迁移，也让「共享」这个
// 语义天然落在原来的位置上。
func DomainDir(base, tag string) string {
	if base == "" || tag == "" {
		return base
	}
	return base + "-" + tag
}

// ValidDomainTag 报告一个可见域标签是否合法：空标签是共享域，其余与插件名同规。
// 标签会被拼进持久化目录（见 DomainDir），所以路径分隔符、点与反斜杠都不允许。
//
// DecideScope 用它挡住插件裁决出来的标签；脚本预置这类绕开裁决的入口也得自己校验。
func ValidDomainTag(tag string) bool {
	return tag == "" || validName.MatchString(tag)
}

// ReadDomains 返回本轮应当读取的可见域标签，本轮写入域排在最前——同名数据在多个域
// 里都存在时，以本轮正在写入的那个为准。
//
// Scope.Read 为 nil 表示不限制，此时枚举 base 同级已存在的全部域，好让「不限制」
// 在这里与 CanRead 的语义一致。
func ReadDomains(base string, sc Scope) []string {
	out := make([]string, 0, 4)
	seen := map[string]bool{}
	add := func(tag string) {
		if !seen[tag] {
			seen[tag] = true
			out = append(out, tag)
		}
	}
	add(sc.Write) // 写入域优先；为空时正好就是共享库
	add("")       // 空标签始终可读
	if sc.Read == nil {
		for _, tag := range existingDomains(base) {
			add(tag)
		}
		return out
	}
	for _, tag := range sc.Read {
		add(tag)
	}
	return out
}

// existingDomains 扫描 base 同级目录，找出所有 <base>-<tag> 形式的可见域标签。
// 读不到目录时返回空——枚举失败应当退化成「只有共享域」，而不是报错阻断调用方。
func existingDomains(base string) []string {
	if base == "" {
		return nil
	}
	parent, prefix := filepath.Dir(base), filepath.Base(base)+"-"
	des, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range des {
		if !de.IsDir() || !strings.HasPrefix(de.Name(), prefix) {
			continue
		}
		// 标签与插件名同规；不合规的目录不是本机制建出来的，不要当成可见域
		if tag := strings.TrimPrefix(de.Name(), prefix); validName.MatchString(tag) {
			out = append(out, tag)
		}
	}
	slices.Sort(out) // 稳定的枚举顺序，让索引与检索结果可预期
	return out
}
