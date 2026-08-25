// Package pathsafe 回答一个问题：这条路径解析完各种链接之后，还在那个目录里吗。
//
// 它只有两个函数，之所以单独成包，是因为**用错的写法看起来是对的**：直接
// filepath.EvalSymlinks 再比前缀，在 Linux 与 macOS 上完全正确，在 Windows 上却
// 形同虚设——EvalSymlinks 不跟随目录联接（junction），而联接恰恰是普通用户在
// Windows 上唯一建得出来的目录链接（符号链接要额外的权限）。这个坑踩过一次：
// 技能扫描与插件包加载各写了一份，两份都只用 EvalSymlinks，越界检查在 Windows 上
// 一直没生效，而测试因为建不出符号链接而跳过，谁都没发现。
package pathsafe

import (
	"os"
	"path/filepath"
	"strings"
)

// Real 把一条路径解析到它真正指向的地方。路径必须存在。
//
// 先 Readlink 再 EvalSymlinks：前者认得符号链接与目录联接，后者负责把剩下的路径段
// 解析干净（比如目录本身就在一条链接下面的情况）。
func Real(p string) (string, error) {
	if target, err := os.Readlink(p); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		p = target
	}
	return filepath.EvalSymlinks(p)
}

// Within 报告 sub 解析之后是否仍落在 root 之内。root 与 sub 都必须存在，
// 解析不了（路径不存在、权限不足）一律按「不在里面」处理——拿不准的时候拒绝。
//
// 两边都要解析：root 自己可能就在一条链接下面（macOS 的 /tmp、把配置目录搬到别的
// 盘），只解析一边会把里面的每一条路径都判成越界。
func Within(root, sub string) bool {
	realRoot, err := Real(root)
	if err != nil {
		return false
	}
	realSub, err := Real(sub)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(realRoot, realSub)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
