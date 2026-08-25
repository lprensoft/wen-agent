package agentplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// StoreDirName 是已安装插件包的存放目录名，位于配置目录之下。
//
// 它与插件的持久化目录（<配置目录>/plugins/<插件名>/）分开：那里放的是插件自己
// 写出来的东西，删掉只丢状态；这里放的是装进来的内容，删掉功能就没了。混在一处
// 的话，「清一下缓存」这种操作会顺手把用户装的插件包一起清掉。
const StoreDirName = "bundles"

// Store 是已安装插件包的存放处。
type Store struct{ dir string }

// NewStore 按配置目录定位存放处。目录可能不存在，由写入方按需创建。
func NewStore(baseDir string) *Store {
	if baseDir == "" {
		return &Store{}
	}
	return &Store{dir: filepath.Join(baseDir, StoreDirName)}
}

// Dir 返回存放目录；为空表示当前没有可用的位置（没有配置目录）。
func (s *Store) Dir() string { return s.dir }

// Path 返回某个插件包应在的目录。name 必须是已经过 checkName 的标准插件名——
// 它的字符集不含分隔符，因此拼不出跳到上级目录的路径。
func (s *Store) Path(name string) string {
	if s.dir == "" {
		return ""
	}
	return filepath.Join(s.dir, name)
}

// List 加载全部已安装的插件包。
//
// 返回的第二个值是「装了但读不出来」的说明，一条一个包。坏掉的包不能静默消失：
// 用户明明装过，界面上却什么都没有，那是最难查的一类问题。
func (s *Store) List() ([]*Bundle, []string) {
	if s.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 一个都没装过，不是问题
		}
		return nil, []string{fmt.Sprintf("读取插件包目录失败：%v", err)}
	}

	var bundles []*Bundle
	var problems []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // 点开头的是我们自己的东西（工具清单缓存）与版本控制目录，不是插件包
		}
		b, err := Load(filepath.Join(s.dir, e.Name()))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s：%v", e.Name(), err))
			continue
		}
		bundles = append(bundles, b)
	}
	// os.ReadDir 已按名字排序，这里再按清单里的名字排一次：目录名与清单名理论上
	// 一致，但用户可以手工把目录改名。注册顺序要稳定——它决定提示词的拼接顺序。
	sort.Slice(bundles, func(i, j int) bool {
		return bundles[i].Manifest.Name < bundles[j].Manifest.Name
	})
	return bundles, problems
}

// Remove 删除一个已安装的插件包。
func (s *Store) Remove(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	path := s.Path(name)
	if path == "" {
		return fmt.Errorf("没有可用的插件包目录")
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("没有装过名为 %q 的插件包", name)
		}
		return err
	}
	return os.RemoveAll(path)
}
