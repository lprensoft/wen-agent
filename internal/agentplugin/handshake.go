package agentplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"wen/internal/i18n"
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
func Handshake(ctx context.Context, lang string, b *Bundle, dataDir string) ([]ToolInfo, []string, error) {
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
		// 声明里带着凭据是个该说出来的问题：插件包是拿来分发的，而分发一份带着
		// 密钥的文件，密钥就等于公开了。只提醒不拦——它是包作者的选择，用户装它
		// 时那个文件就在手边，拦下来只会变成一次没有解释的失败。
		if h := credentialHeaders(s.Headers); len(h) > 0 {
			problems = append(problems, i18n.Tf(lang, "plugin.bundle.warn.inline_secret",
				"%s：mcp.json 里直接写了 %s，插件包是公开分发的，别把凭据放在里面",
				s.Name, strings.Join(h, i18n.T(lang, "plugin.bundle.sep", "、"))))
		}

		c := newClient(lang, resolved)
		remote, err := c.listTools(ctx)
		c.Close() // 握手完就放掉：真正用的时候会按需重新拉起
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s：%v", s.Name, err))
			continue
		}

		for _, rt := range remote {
			local := LocalToolName(b.Manifest.Name, s.Name, rt.Name)
			if seen[local] {
				problems = append(problems, i18n.Tf(lang, "plugin.bundle.warn.dup_tool",
					"%s 的 %s：与同包里另一件工具重名，已跳过", s.Name, rt.Name))
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
			problems = append(problems, i18n.Tf(lang, "plugin.bundle.warn.too_many_tools",
				"%s：工具超过 %d 件，只收下了前面这些——清单每一轮都要随请求发出去", s.Name, maxToolsPerServer))
		}
	}

	// 顺序稳定：工具清单每轮都进上下文，顺序一变提示词缓存就整段作废
	sort.Slice(tools, func(i, j int) bool { return tools[i].Local < tools[j].Local })
	return tools, problems, nil
}

// credentialHeaders 挑出声明里那些一看就是凭据的头。
func credentialHeaders(headers map[string]string) []string {
	var out []string
	for k := range headers {
		switch strings.ToLower(k) {
		case "authorization", "proxy-authorization", "cookie", "x-api-key":
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
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
