package agentplugin

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MCPSchemaURL 是本实现支持的 mcp.json schema。
const MCPSchemaURL = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"

// 传输方式。标准要求客户端至少支持 stdio 与 streamable-http 之一；
// sse 是 2024-11-05 那版协议留下的，已废弃，支持与否随意。
const (
	TransportStdio = "stdio"
	TransportHTTP  = "streamable-http"
	TransportSSE   = "sse"
)

// 插件包能用的两个占位符。它们由客户端提供，也因此不允许包自己在 env 里定义——
// 那等于让包把自己的根目录指到别处去。
const (
	PlaceholderRoot = "${PLUGIN_ROOT}"
	PlaceholderData = "${PLUGIN_DATA}"
	EnvRoot         = "PLUGIN_ROOT"
	EnvData         = "PLUGIN_DATA"
)

// MCPServer 是 mcp.json 里的一台 server，字段尚未展开占位符。
type MCPServer struct {
	Name    string
	Type    string
	Command string
	Args    []string
	Env     map[string]string
	Cwd     string
	URL     string
	Headers map[string]string
}

// mcpFile 是 mcp.json 的线格式。
type mcpFile struct {
	Schema  string                     `json:"$schema"`
	Servers map[string]json.RawMessage `json:"mcpServers"`
}

type mcpServerJSON struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// ParseMCP 解析 mcp.json。
//
// 失败的边界是标准划好的，两档分明：整份文件读不出来或 schema 不认识 → 这个包的
// MCP 组件整个作废（返回 error），但技能组件照常；单台 server 的字段不对或传输方式
// 不支持 → 只跳过那一台，其余照常（记一条 warning）。一台 server 写错不该让同一个
// 包里另外两台一起失效。
func ParseMCP(data []byte) ([]MCPServer, []string, error) {
	return parseMCP(data, true)
}

// ParseMCPLoose 解析别家格式的 MCP 声明：内容形状一样，但没有 $schema 那一行
// （那是 Agent Plugins 加的）。除此之外的校验一视同仁。
func ParseMCPLoose(data []byte) ([]MCPServer, []string, error) {
	return parseMCP(data, false)
}

func parseMCP(data []byte, strict bool) ([]MCPServer, []string, error) {
	var f mcpFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("MCP 声明不是合法的 JSON：%w", err)
	}
	if strict {
		if f.Schema == "" {
			return nil, nil, fmt.Errorf("%s 里缺少 $schema", MCPFile)
		}
		if f.Schema != MCPSchemaURL {
			return nil, nil, fmt.Errorf("不支持的 mcp.json $schema %q（本程序支持 %s）", f.Schema, MCPSchemaURL)
		}
	}

	// 名字排序，注册顺序因此稳定——工具清单要拼进上下文，顺序变了缓存就白费
	names := make([]string, 0, len(f.Servers))
	for name := range f.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	var servers []MCPServer
	var warnings []string
	for _, name := range names {
		s, err := parseServer(name, f.Servers[name])
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("MCP 声明里的 %s 已跳过：%v", name, err))
			continue
		}
		servers = append(servers, s)
	}
	return servers, warnings, nil
}

func parseServer(name string, raw json.RawMessage) (MCPServer, error) {
	var j mcpServerJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return MCPServer{}, fmt.Errorf("字段类型不对——%v", err)
	}
	s := MCPServer{
		Name: name, Type: j.Type, Command: j.Command, Args: j.Args,
		Env: j.Env, Cwd: j.Cwd, URL: j.URL, Headers: j.Headers,
	}
	switch j.Type {
	case TransportStdio:
		return s, checkStdio(s)
	case TransportHTTP, TransportSSE:
		return s, checkHTTP(s)
	case "":
		return MCPServer{}, fmt.Errorf("没有写 type")
	default:
		return MCPServer{}, fmt.Errorf("不认识的 type %q", j.Type)
	}
}

func checkStdio(s MCPServer) error {
	if s.Command == "" {
		return fmt.Errorf("stdio 需要 command")
	}
	// 「单个词」是标准的要求，不是本实现的洁癖：允许一整行命令就等于允许 shell
	// 语法，而参数该走 args。带空格的 command 多半是把命令连参数写在了一起。
	if strings.ContainsAny(s.Command, " \t") {
		return fmt.Errorf("command 只能是一个词，参数写到 args 里")
	}
	if strings.Contains(s.Command, PlaceholderRoot) || strings.Contains(s.Command, PlaceholderData) {
		return fmt.Errorf("command 里不做占位符展开")
	}
	for k := range s.Env {
		if k == EnvRoot || k == EnvData {
			return fmt.Errorf("env 里不能自己定义 %s", k)
		}
	}
	return nil
}

func checkHTTP(s MCPServer) error {
	if s.URL == "" {
		return fmt.Errorf("%s 需要 url", s.Type)
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		return fmt.Errorf("url 解析失败——%v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url 只能是 http 或 https")
	}
	if u.User != nil {
		return fmt.Errorf("url 里不能带用户名密码")
	}
	if u.Fragment != "" {
		return fmt.Errorf("url 里不能带 # 片段")
	}
	// 非回环地址必须走 HTTPS：明文发出去的每一次调用都可能带着上下文里的内容
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("非本机地址必须用 https")
	}
	return nil
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]"
}

// ResolvedServer 是展开占位符之后、可以真正拿去启动的一台 server。
type ResolvedServer struct {
	Name    string
	Type    string
	Command string // 绝对路径或裸命令名
	Args    []string
	Env     map[string]string // 只含包自己声明的，加上 PLUGIN_ROOT / PLUGIN_DATA
	Dir     string
	URL     string
	Headers map[string]string
}

// Resolve 展开占位符并校验路径。
//
// 展开只发生在 args 的每一项、env 的值与 cwd 上——command 与各组件的固定位置不展开，
// env 的键也不展开。替换是一次性的、非递归的：展开结果里再出现 ${PLUGIN_ROOT} 不会
// 被再展开一次，认不出来的占位符样子的文本原样留着。
func (s MCPServer) Resolve(root Root, dataDir string) (ResolvedServer, error) {
	if dataDir == "" {
		return ResolvedServer{}, fmt.Errorf("没有可用的数据目录")
	}
	// PLUGIN_DATA 由客户端负责创建、可写、且跨更新保留
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return ResolvedServer{}, fmt.Errorf("创建数据目录失败：%w", err)
	}
	expand := func(v string) string {
		v = strings.ReplaceAll(v, PlaceholderRoot, root.Path())
		return strings.ReplaceAll(v, PlaceholderData, dataDir)
	}

	out := ResolvedServer{
		Name: s.Name, Type: s.Type, Command: s.Command,
		URL: s.URL, Headers: s.Headers, Dir: root.Path(),
	}
	for _, a := range s.Args {
		out.Args = append(out.Args, expand(a))
	}
	out.Env = map[string]string{EnvRoot: root.Path(), EnvData: dataDir}
	for k, v := range s.Env {
		out.Env[k] = expand(v)
	}

	if s.Type != TransportStdio {
		return out, nil
	}

	// 以 ./ 开头的 command 是包内的可执行文件，必须落在包目录之内；
	// 裸命令名交给 PATH 去找，那是用户机器上的事，与包无关。
	if strings.HasPrefix(s.Command, "./") || strings.HasPrefix(s.Command, ".\\") {
		p := root.Join(filepath.FromSlash(strings.TrimPrefix(s.Command, "./")))
		real, err := root.Contains(p)
		if err != nil {
			return ResolvedServer{}, fmt.Errorf("command %q 不可用：%w", s.Command, err)
		}
		out.Command = real
	}

	if s.Cwd != "" {
		dir := expand(s.Cwd)
		if !filepath.IsAbs(dir) {
			dir = root.Join(filepath.FromSlash(dir))
		}
		real, err := root.Contains(dir)
		if err != nil {
			return ResolvedServer{}, fmt.Errorf("cwd %q 不可用：%w", s.Cwd, err)
		}
		out.Dir = real
	}
	return out, nil
}

// EnvList 把环境变量拼成 exec.Cmd 要的形式，在当前进程的环境之上叠加。
//
// 叠加而不是只给这几个：MCP server 多半要靠 PATH 找解释器、靠 HOME 找配置，
// 给一个空环境的话，绝大多数 server 根本起不来。
func (r ResolvedServer) EnvList() []string {
	keys := make([]string, 0, len(r.Env))
	for k := range r.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := os.Environ()
	for _, k := range keys {
		out = append(out, k+"="+r.Env[k])
	}
	return out
}
