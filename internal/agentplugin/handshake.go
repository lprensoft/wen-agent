package agentplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
)

// Handshake 连一遍插件包里的每台 server，取回工具清单并落盘。
//
// 这一步在安装时做一次，此后由「重新握手」按钮或 wen plugins refresh 重做。之所以
// 不在每次启动时现问：插件系统注册插件时就要拿到工具清单（它得查全局重名），而那一步
// 在 Init 之前、且握着写锁——在那里为每台 server 起一个子进程做握手，等于让服务在启动
// 时停上好几秒，装得越多停得越久。
//
// dataDir 是交给 server 的 PLUGIN_DATA。握手用的是与真正运行时同一份参数，否则「握手
// 时好好的、一调用就不行」这种问题会很难查。
func Handshake(ctx context.Context, b *Bundle, dataDir string) ([]ToolInfo, []string, error) {
	if len(b.Servers) == 0 {
		return nil, nil, nil
	}
	var tools []ToolInfo
	var problems []string
	seen := map[string]bool{}

	for _, s := range b.Servers {
		resolved, err := s.Resolve(b.Root, dataDir)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s：%v", s.Name, err))
			continue
		}
		if resolved.Type != TransportStdio {
			problems = append(problems, fmt.Sprintf("%s：本版只支持 stdio，%s 暂不支持", s.Name, resolved.Type))
			continue
		}

		c := newClient(resolved)
		remote, err := c.listTools(ctx)
		c.Close() // 握手完就放掉：真正用的时候会按需重新拉起
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s：%v", s.Name, err))
			continue
		}

		for _, rt := range remote {
			local := LocalToolName(b.Manifest.Name, s.Name, rt.Name)
			if seen[local] {
				problems = append(problems, fmt.Sprintf("%s 的 %s：与同包里另一件工具重名，已跳过", s.Name, rt.Name))
				continue
			}
			seen[local] = true

			var schema json.RawMessage
			if rt.InputSchema != nil {
				if raw, err := json.Marshal(rt.InputSchema); err == nil {
					schema = raw
				}
			}
			tools = append(tools, ToolInfo{
				Server: s.Name, Remote: rt.Name, Local: local,
				Description: rt.Description, Schema: schema,
			})
		}
		if len(remote) >= maxToolsPerServer {
			problems = append(problems, fmt.Sprintf("%s：工具超过 %d 件，只收下了前面这些——清单每一轮都要随请求发出去", s.Name, maxToolsPerServer))
		}
	}

	// 顺序稳定：工具清单每轮都进上下文，顺序一变提示词缓存就整段作废
	sort.Slice(tools, func(i, j int) bool { return tools[i].Local < tools[j].Local })
	return tools, problems, nil
}

// DataDir 返回某个插件包的 PLUGIN_DATA 目录。
//
// 它落在插件自己的持久化目录下（<配置目录>/plugins/<插件名>/data），而不是插件包目录
// 里：标准要求这个目录跨更新保留，而包目录在卸载重装时会被整个换掉。
func DataDir(baseDir, manifestName string) string {
	if baseDir == "" {
		return ""
	}
	return filepath.Join(baseDir, "plugins", PluginName(manifestName), DataDirName)
}
