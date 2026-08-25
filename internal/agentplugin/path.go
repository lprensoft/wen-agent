package agentplugin

import (
	"fmt"
	"os"
	"path/filepath"

	"wen/internal/pathsafe"
)

// Root 是一个已经解析过符号链接的插件包根目录。
//
// 之所以把它做成一个类型而不是到处传字符串：标准要求插件包里的**每一条**路径都
// 必须落在根目录之内，符号链接与 junction 逃出去的要拒绝。而正确的做法是先把根
// 自己解析一遍再比——如果根本身就在一条符号链接下面（macOS 的 /tmp、Windows 上
// 把配置目录搬到别的盘），拿没解析的根去比，包里每一条路径都会被判成越界。
type Root struct{ path string }

// NewRoot 解析插件包根目录。目录不存在时报错。
func NewRoot(dir string) (Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Root{}, err
	}
	real, err := pathsafe.Real(abs)
	if err != nil {
		return Root{}, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return Root{}, err
	}
	if !info.IsDir() {
		return Root{}, fmt.Errorf("%s 不是目录", dir)
	}
	return Root{path: real}, nil
}

// Path 返回解析后的根目录。
func (r Root) Path() string { return r.path }

// Join 拼出根目录下的一条路径，不检查它存不存在。
func (r Root) Join(elem ...string) string {
	return filepath.Join(append([]string{r.path}, elem...)...)
}

// Contains 解析 p 并确认它落在根目录之内，返回解析后的路径。
//
// 越界一律报错，由调用方按「最窄边界」决定拒到哪一层：清单越界拒整个插件包，
// 某个技能目录越界只跳过那一个技能。这条分工是标准写死的——一个坏掉的技能不该
// 让整包装不上。
func (r Root) Contains(p string) (string, error) {
	real, err := pathsafe.Real(p)
	if err != nil {
		return "", err
	}
	if !pathsafe.Within(r.path, p) {
		return "", fmt.Errorf("%s 指向插件包目录之外，已拒绝", p)
	}
	return real, nil
}
