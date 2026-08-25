package agentplugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"wen/internal/plugin"
)

const mcpManifest = `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "demo",
  "description": "A demo bundle"
}`

func mcpJSON(body string) string {
	return `{"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
		"mcpServers": ` + body + "}"
}

func TestParseMCPGood(t *testing.T) {
	servers, warnings, err := ParseMCP([]byte(mcpJSON(`{
		"b": {"type": "stdio", "command": "second"},
		"a": {"type": "stdio", "command": "first", "args": ["--root", "${PLUGIN_ROOT}"]}
	}`)))
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("不该有提醒: %v", warnings)
	}
	// 顺序必须稳定：工具清单每轮都进上下文，顺序一变提示词缓存整段作废
	if len(servers) != 2 || servers[0].Name != "a" || servers[1].Name != "b" {
		t.Fatalf("没有按名字排序: %+v", servers)
	}
}

// 整份文件坏掉与单台 server 坏掉是两档：前者作废整个 MCP 组件，后者只跳过那一台。
func TestParseMCPFailureBoundaries(t *testing.T) {
	if _, _, err := ParseMCP([]byte(`{"mcpServers": {}}`)); err == nil {
		t.Error("缺 $schema 应当整份作废")
	}
	if _, _, err := ParseMCP([]byte(`{"$schema": "https://agent-plugins.org/schemas/9.9/mcp.schema.json"}`)); err == nil {
		t.Error("schema 版本不认识应当整份作废")
	}

	servers, warnings, err := ParseMCP([]byte(mcpJSON(`{
		"good": {"type": "stdio", "command": "ok"},
		"nocmd": {"type": "stdio"},
		"weird": {"type": "carrier-pigeon"},
		"spaced": {"type": "stdio", "command": "npx -y thing"},
		"insecure": {"type": "streamable-http", "url": "http://example.com/mcp"},
		"creds": {"type": "streamable-http", "url": "https://u:p@example.com/mcp"},
		"envclash": {"type": "stdio", "command": "ok", "env": {"PLUGIN_ROOT": "/tmp"}}
	}`)))
	if err != nil {
		t.Fatalf("单台坏掉不该让整份作废: %v", err)
	}
	if len(servers) != 1 || servers[0].Name != "good" {
		t.Fatalf("该只留下 good，实际: %+v", servers)
	}
	if len(warnings) != 6 {
		t.Fatalf("六台各该报一句，实际 %d 条: %v", len(warnings), warnings)
	}
}

// 回环地址允许明文：本机上的 server 没有中间人，而要求它配证书等于没法用。
func TestParseMCPLoopbackHTTPAllowed(t *testing.T) {
	servers, _, err := ParseMCP([]byte(mcpJSON(`{
		"local": {"type": "streamable-http", "url": "http://127.0.0.1:3000/mcp"}
	}`)))
	if err != nil || len(servers) != 1 {
		t.Fatalf("本机明文该放行: %v %+v", err, servers)
	}
}

func TestResolveExpansion(t *testing.T) {
	dir := t.TempDir()
	root, err := NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")

	s := MCPServer{
		Name: "x", Type: TransportStdio, Command: "thing",
		Args: []string{"--root", PlaceholderRoot, "--data", PlaceholderData + "/db", "--plain", "${NOPE}"},
		Env:  map[string]string{"CONFIG": PlaceholderRoot + "/config.json"},
	}
	got, err := s.Resolve(root, data)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if got.Args[1] != root.Path() {
		t.Errorf("PLUGIN_ROOT 没展开: %q", got.Args[1])
	}
	if !strings.HasPrefix(got.Args[3], data) {
		t.Errorf("PLUGIN_DATA 没展开: %q", got.Args[3])
	}
	// 认不出来的占位符样子的文本原样留着，不是清空也不是报错
	if got.Args[5] != "${NOPE}" {
		t.Errorf("不认识的占位符该原样留着: %q", got.Args[5])
	}
	if got.Env[EnvRoot] != root.Path() || got.Env[EnvData] != data {
		t.Errorf("两个环境变量该由客户端给出: %+v", got.Env)
	}
	// PLUGIN_DATA 必须是真的建好的：标准要求它可写
	if _, err := os.Stat(data); err != nil {
		t.Errorf("数据目录没建出来: %v", err)
	}
}

// 包内的可执行文件不得指到包外。
func TestResolveCommandContainment(t *testing.T) {
	dir := t.TempDir()
	root, err := NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := MCPServer{Name: "x", Type: TransportStdio, Command: "./bin/nope"}
	if _, err := s.Resolve(root, filepath.Join(dir, "data")); err == nil {
		t.Error("包里没有这个文件，应当报错而不是照样启动")
	}
}

func TestLocalToolName(t *testing.T) {
	// 前缀带包名：两个包各带一台叫 fs 的 server 是正常的，只按 server 前缀的话
	// 后装的那个包会因为工具重名而整个注册失败
	a := LocalToolName("pkg-a", "fs", "read")
	b := LocalToolName("pkg-b", "fs", "read")
	if a == b {
		t.Fatalf("不同包的同名工具撞到了一起: %s", a)
	}
	// 连字符保留：模型接口那边 [a-zA-Z0-9_-] 都合法，而插件名那条更严的规则
	// （要拼进目录名）管不到工具名——留着连字符，包名在工具名里还认得出来
	if a != "pkg-a_fs_read" {
		t.Errorf("清洗结果不对: %s", a)
	}
	// 远端的名字是外部数据，什么字符都可能出现
	if got := LocalToolName("p", "s", "weird name?+"); strings.ContainsAny(got, " ?+") {
		t.Errorf("非法字符没清干净: %s", got)
	}
	long := LocalToolName(strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40))
	if len(long) > maxToolNameLen {
		t.Errorf("超长名字没截断: %d", len(long))
	}
}

func TestToolCacheRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir())
	want := []ToolInfo{{Server: "fs", Remote: "read", Local: "demo_fs_read", Description: "d"}}
	if err := store.SaveTools("demo", want); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadTools("demo")
	if err != nil || len(got) != 1 || got[0].Local != "demo_fs_read" {
		t.Fatalf("读回来不对: %v %+v", err, got)
	}
	// 缓存目录是点开头的，不能被当成插件包
	if list, problems := store.List(); len(list) != 0 || len(problems) != 0 {
		t.Fatalf("缓存目录被当成插件包了: %d %v", len(list), problems)
	}
	store.DropTools("demo")
	if got, _ := store.LoadTools("demo"); len(got) != 0 {
		t.Error("卸载之后缓存还在")
	}
}

// 没握过手不是故障，只是清单还空着——M1 装的包、手工拷进来的包都是这样。
func TestLoadToolsMissingIsNotAnError(t *testing.T) {
	store := NewStore(t.TempDir())
	got, err := store.LoadTools("never-installed")
	if err != nil || got != nil {
		t.Fatalf("缺缓存该安静地返回空: %v %+v", err, got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("短", 100); got != "短" {
		t.Errorf("没超限不该动: %q", got)
	}
	long := strings.Repeat("中", 100) // 每个 3 字节
	got := truncate(long, 50)
	if !strings.Contains(got, "已截断") {
		t.Error("截断了却没说")
	}
	// 不能把多字节字符切成半个
	if !utf8.ValidString(got) {
		t.Error("截断处切坏了字符")
	}
}

func TestPluginContracts(t *testing.T) {
	var p any = &Plugin{}
	checks := map[string]bool{}
	_, checks["Plugin"] = p.(plugin.Plugin)
	_, checks["Configurable"] = p.(plugin.Configurable)
	_, checks["Actionable"] = p.(plugin.Actionable)
	_, checks["Stoppable"] = p.(plugin.Stoppable)
	_, checks["StatusReporter"] = p.(plugin.StatusReporter)
	_, checks["Categorized"] = p.(plugin.Categorized)
	for name, ok := range checks {
		if !ok {
			t.Errorf("没实现 %s", name)
		}
	}
}

// ---------- 端到端：真的起一台 MCP server ----------

// 测试进程带上这个环境变量再启动一次，就扮演一台 MCP server。拿自己当对手方，
// 好过为测试引一个外部依赖——而这条路径（拉起子进程、握手、取清单、调用）恰恰是
// 最需要真的跑一遍的。
const serverEnv = "WEN_TEST_MCP_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(serverEnv) != "" {
		runFakeServer()
		return
	}
	os.Exit(m.Run())
}

func runFakeServer() {
	type args struct {
		Text string `json:"text" jsonschema:"要回显的文字"`
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "把给它的文字原样送回来"},
		func(_ context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "收到：" + a.Text}}}, nil, nil
		})
	// 标准要求客户端把这两个变量交给子进程，所以让子进程自己报一遍——
	// 只在这边断言 EnvList 拼得对，证明不了它真的到了对面
	mcp.AddTool(s, &mcp.Tool{Name: "where", Description: "报出客户端给的两个目录"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			text := os.Getenv(EnvRoot) + "|" + os.Getenv(EnvData)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
	_ = s.Run(context.Background(), &mcp.StdioTransport{})
}

func TestEndToEndStdio(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	self := filepath.ToSlash(os.Args[0])
	writeBundle(t, src, mcpManifest, nil, mcpJSON(`{
		"self": {"type": "stdio", "command": "`+self+`", "env": {"`+serverEnv+`": "1"}}
	}`))

	store := NewStore(filepath.Join(base, "cfg"))
	b, err := store.Install(src)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if len(b.Servers) != 1 {
		t.Fatalf("mcp.json 没读出来: %+v %v", b.Servers, b.Problems)
	}

	// 握手：拉起子进程、initialize、tools/list
	tools, problems, err := Handshake(context.Background(), b, filepath.Join(base, "data"))
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("握手报了问题: %v", problems)
	}
	if len(tools) != 2 || tools[0].Local != "demo_self_echo" || tools[1].Local != "demo_self_where" {
		t.Fatalf("工具清单不对: %+v", tools)
	}
	if tools[0].Description == "" || len(tools[0].Schema) == 0 {
		t.Errorf("描述与 schema 该原样带回来: %+v", tools[0])
	}
	if err := store.SaveTools(b.Manifest.Name, tools); err != nil {
		t.Fatal(err)
	}

	// 装配成插件，走一次真正的调用
	b2, err := Load(store.Path(b.Manifest.Name))
	if err != nil {
		t.Fatal(err)
	}
	p := NewPlugin(b2, store)
	if got := len(p.Tools()); got != 2 {
		t.Fatalf("Tools() 该从缓存里读出 2 件，实际 %d 件", got)
	}
	if err := p.Init(plugin.InitContext{StateDir: filepath.Join(base, "state")}, nil); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	t.Cleanup(p.Stop)

	out, err := p.Tools()[0].Execute(context.Background(), json.RawMessage(`{"text": "喂"}`))
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !strings.Contains(out, "收到：喂") {
		t.Fatalf("返回内容不对: %q", out)
	}

	// 两个目录必须真的到了子进程手里
	where, err := p.Tools()[1].Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("调用 where 失败: %v", err)
	}
	root, data, _ := strings.Cut(where, "|")
	if root != b2.Root.Path() {
		t.Errorf("子进程看到的 PLUGIN_ROOT 不对: %q，想要 %q", root, b2.Root.Path())
	}
	if !strings.HasSuffix(filepath.Clean(data), filepath.Join("state", DataDirName)) {
		t.Errorf("子进程看到的 PLUGIN_DATA 不对: %q", data)
	}
}

// 开着「调用前确认」而当前轮次没有可确认的人时，必须按拒绝处理——
// 拿不到答复不等于得到许可。
func TestConfirmRequiredWithoutConfirmerRefuses(t *testing.T) {
	p := &Plugin{
		name:    "ext_demo",
		bundle:  &Bundle{Manifest: Manifest{Name: "demo"}},
		clients: map[string]*client{"s": {}},
		confirm: true,
		ready:   true,
	}
	tool := &remoteTool{p: p, info: ToolInfo{Server: "s", Remote: "x", Local: "demo_s_x"}}
	if _, err := tool.Execute(context.Background(), nil); err == nil {
		t.Fatal("没有确认通道却放行了")
	} else if !strings.Contains(err.Error(), "拒绝") {
		t.Errorf("该说清是按拒绝处理: %v", err)
	}
}

// ---------- 端到端：Streamable HTTP ----------

// 起一台本机的 HTTP MCP server，走完与 stdio 同一条链路。顺带验两件只有 HTTP 才有的
// 事：声明的头真的发了出去，以及回环地址允许明文（对外必须 HTTPS 已由解析层挡住）。
func TestEndToEndStreamableHTTP(t *testing.T) {
	gotHeader := make(chan string, 8)
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-http", Version: "v0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ping", Description: "回一句 pong"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotHeader <- r.Header.Get("X-Tenant"):
		default:
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBundle(t, src, mcpManifest, nil, mcpJSON(`{
		"remote": {"type": "streamable-http", "url": "`+ts.URL+`", "headers": {"X-Tenant": "public"}}
	}`))

	store := NewStore(filepath.Join(base, "cfg"))
	b, err := store.Install(src)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	tools, problems, err := Handshake(context.Background(), b, filepath.Join(base, "data"))
	if err != nil || len(problems) > 0 {
		t.Fatalf("握手失败: %v %v", err, problems)
	}
	if len(tools) != 1 || tools[0].Local != "demo_remote_ping" {
		t.Fatalf("工具清单不对: %+v", tools)
	}
	if err := store.SaveTools(b.Manifest.Name, tools); err != nil {
		t.Fatal(err)
	}

	p := NewPlugin(b, store)
	if err := p.Init(plugin.InitContext{StateDir: filepath.Join(base, "state")}, nil); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	t.Cleanup(p.Stop)

	out, err := p.Tools()[0].Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !strings.Contains(out, "pong") {
		t.Fatalf("返回内容不对: %q", out)
	}
	select {
	case h := <-gotHeader:
		if h != "public" {
			t.Errorf("声明的头没发出去: %q", h)
		}
	default:
		t.Error("一个请求都没收到")
	}
}

// 插件包是公开分发的，写死在 mcp.json 里的凭据等于公开的凭据。提醒但不拦——
// 那是包作者的选择，拦下来只会变成一次没有解释的失败。
func TestShippedCredentialWarns(t *testing.T) {
	if got := credentialHeaders(map[string]string{"X-Tenant": "public"}); len(got) != 0 {
		t.Errorf("普通的头不该报: %v", got)
	}
	got := credentialHeaders(map[string]string{"Authorization": "Bearer x", "cookie": "a=b"})
	if len(got) != 2 {
		t.Errorf("凭据类的头该都挑出来（大小写不敏感）: %v", got)
	}
}

// ---------- 别家格式的插件包 ----------

// Claude / Codex / Cursor 的插件包比 Agent Plugins 出现得早，社区里已经有很多。
// 它们的清单在别的位置、没有 $schema，但内容的固定位置是一样的——认下来，用户就
// 不必先手工改一个 plugin.json 才能装。
func TestLoadClaudeLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pkg")
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"),
		[]byte(`{"name": "legacy-pack", "description": "从别处拿来的", "version": "2.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// 别家的 MCP 声明在 .mcp.json，且没有 $schema
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"),
		[]byte(`{"mcpServers": {"x": {"type": "stdio", "command": "thing"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBundle(t, dir, "", map[string]string{"a": "---\nname: a\ndescription: d\n---\n"}, "")

	b, err := Load(dir)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if b.Format != "Claude" {
		t.Errorf("格式认错了: %q", b.Format)
	}
	if b.Manifest.Name != "legacy-pack" || b.Manifest.Version != "2.0" {
		t.Errorf("清单没读对: %+v", b.Manifest)
	}
	if b.SkillsDir == "" {
		t.Error("技能目录没认出来")
	}
	if len(b.Servers) != 1 {
		t.Errorf(".mcp.json 没读出来: %+v", b.Servers)
	}
	// 读得懂多少要说清楚：那些格式特有的东西（命令、子代理、钩子）本程序不认识，
	// 不说一句的话，用户会以为整个包都生效了
	var told bool
	for _, p := range b.Problems {
		if strings.Contains(p, "只读得懂") {
			told = true
		}
	}
	if !told {
		t.Errorf("该说清只读得懂哪些部分: %v", b.Problems)
	}
}

// 名字仍按 Agent Plugins 的规则校验——它要拼进目录名与插件名，这条不能松。
func TestLooseManifestStillChecksName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pkg")
	if err := os.MkdirAll(filepath.Join(dir, ".cursor-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".cursor-plugin", "plugin.json"),
		[]byte(`{"name": "Bad Name"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("名字不合法却装上了")
	}
}
