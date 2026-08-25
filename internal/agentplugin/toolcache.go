package agentplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CacheDirName 是工具清单缓存所在的目录，与各插件包平级。
//
// 点开头，因此 Store.List 会跳过它——缓存不是插件包。放在包外面而不是包里面，是为了
// 让插件包目录保持成「装进来时那个样子」：包内多一个我们自己写的文件，下次比对内容、
// 校验签名或做增量更新时都要先记得把它排除掉。
const CacheDirName = ".tools"

// ToolInfo 是一件远端工具的登记。
type ToolInfo struct {
	Server      string          `json:"server"`      // 来自哪台 server
	Remote      string          `json:"remote"`      // 远端的工具名，调用时用它
	Local       string          `json:"local"`       // 本地暴露给模型的名字（已前缀化）
	Description string          `json:"description"` // 远端给的说明，外部数据
	Schema      json.RawMessage `json:"schema"`      // 参数 JSON Schema，原样透传
}

// toolCache 是一次握手的产物。
type toolCache struct {
	Bundle string     `json:"bundle"`
	Tools  []ToolInfo `json:"tools"`
}

// 工具名必须过得了模型接口那一关：Anthropic 侧限定 [a-zA-Z0-9_-]{1,64}。
// 远端的名字是外部数据，什么都可能写，所以一律清洗一遍。
var badToolChar = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

const maxToolNameLen = 64

// LocalToolName 把一件远端工具的名字前缀化成本地唯一的名字。
//
// 前缀带上插件包名而不只是 server 名：插件系统的工具名是**全局**唯一的，撞名会让
// 整个插件注册失败。两个包各带一台叫 fs 的 server 是完全正常的事，只按 server 前缀
// 的话，后装的那个包会整个用不了——而包名由存放目录保证唯一。
func LocalToolName(bundle, server, tool string) string {
	clean := func(s string) string {
		return strings.Trim(badToolChar.ReplaceAllString(s, "_"), "_")
	}
	name := clean(bundle) + "_" + clean(server) + "_" + clean(tool)
	if len(name) <= maxToolNameLen {
		return name
	}
	// 截断会制造撞名，所以补一段原名的哈希：截断后的前缀一样、原名不同的两件
	// 工具，哈希不同，仍然区分得开。
	sum := sha256.Sum256([]byte(name))
	return name[:maxToolNameLen-8] + "_" + hex.EncodeToString(sum[:])[:7]
}

// cachePath 返回某个插件包的工具清单缓存位置。
func (s *Store) cachePath(bundle string) string {
	if s.dir == "" {
		return ""
	}
	return filepath.Join(s.dir, CacheDirName, bundle+".json")
}

// LoadTools 读出某个插件包缓存下来的工具清单。
//
// 缓存不存在不是错误，只是「还没握过手」：M1 装的包没有缓存，手工拷进来的包也没有。
// 调用方据此提示用户刷新一次，而不是当成故障。
func (s *Store) LoadTools(bundle string) ([]ToolInfo, error) {
	path := s.cachePath(bundle)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c toolCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("工具清单缓存已损坏：%w", err)
	}
	return c.Tools, nil
}

// SaveTools 写下一次握手的结果。
//
// 之所以要落盘而不是每次启动现问：插件系统在注册插件时就要拿到工具清单（它得查
// 全局重名），而那一步在 Init 之前、且握着写锁。在那里拉起子进程做一次握手，等于
// 让整个服务在启动时为每台 server 停上几秒——装了十台就是十次。
func (s *Store) SaveTools(bundle string, tools []ToolInfo) error {
	path := s.cachePath(bundle)
	if path == "" {
		return fmt.Errorf("没有可用的缓存位置")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(toolCache{Bundle: bundle, Tools: tools}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// DropTools 删掉某个插件包的缓存，卸载时用。
func (s *Store) DropTools(bundle string) {
	if path := s.cachePath(bundle); path != "" {
		_ = os.Remove(path)
	}
}
