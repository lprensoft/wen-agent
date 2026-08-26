// 版本资源与程序图标：改动 internal/version 或 logo 图源后执行 go generate ./cmd/wen
// 重新生成 .syso（生成器说明见 tools/genwinres 与 tools/genicon），go build 会自动
// 链入本目录的 .syso。两条指令有先后依赖——genicon 先出 .ico，genwinres 才编得进去。
//go:generate go run wen/tools/genicon
//go:generate go run wen/tools/genwinres

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"wen/internal/agentplugin"
	"wen/internal/config"
	"wen/internal/plugin"
	"wen/internal/plugin/builtin/agenda"
	"wen/internal/plugin/builtin/belongings"
	"wen/internal/plugin/builtin/bodysense"
	"wen/internal/plugin/builtin/bundles"
	"wen/internal/plugin/builtin/dualpersona"
	"wen/internal/plugin/builtin/execcmd"
	"wen/internal/plugin/builtin/health"
	"wen/internal/plugin/builtin/heartbeat"
	"wen/internal/plugin/builtin/larkbot"
	"wen/internal/plugin/builtin/memory"
	"wen/internal/plugin/builtin/mood"
	"wen/internal/plugin/builtin/people"
	"wen/internal/plugin/builtin/presence"
	"wen/internal/plugin/builtin/qqbot"
	"wen/internal/plugin/builtin/readfile"
	"wen/internal/plugin/builtin/relationship"
	"wen/internal/plugin/builtin/roleplay"
	"wen/internal/plugin/builtin/scene"
	"wen/internal/plugin/builtin/scheduler"
	"wen/internal/plugin/builtin/selfupdate"
	"wen/internal/plugin/builtin/sessionsearch"
	"wen/internal/plugin/builtin/skills"
	"wen/internal/plugin/builtin/stylewatch"
	"wen/internal/plugin/builtin/telegrambot"
	"wen/internal/plugin/builtin/unspoken"
	"wen/internal/plugin/builtin/weather"
	"wen/internal/plugin/builtin/webfetch"
	"wen/internal/plugin/builtin/wechatbot"
	"wen/internal/runlock"
	"wen/internal/server"
	"wen/internal/updater"
	"wen/internal/version"
)

// runServe 启动服务。这是没有子命令时的默认动作，也是 wen serve 的实现。
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("c", "", tr("cli.flag.config", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）"))
	port := fs.Int("p", 0, tr("cli.serve.flag.port", "监听端口（覆盖配置文件）"))
	_ = fs.Parse(args)

	path := config.ResolvePath(*configPath)
	cfg, err := config.Load(path)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if *port != 0 {
		cfg.Server.Port = *port
	}
	// 语言要在第一行日志之前定下——启动日志本身就是要跟着它走的东西之一
	adoptLang(cfg)

	// 可执行文件的位置在这里就取好，不等到要重启时再问。
	// Linux 上 /proc/self/exe 在文件被替换之后会变成「……(deleted)」，而自更新做的
	// 正是替换这个文件——启动时取到的才是那个还能拿去 exec 的路径。
	exePath, exeErr := updater.ExePath()
	if exeErr != nil {
		log.Printf("提示: 无法定位当前程序（%v），自更新与重启将不可用", exeErr)
	}

	// restartCh 传「换好新版了，可以重启」。缓冲 1 且非阻塞投递：重启只需要发生一次。
	restartCh := make(chan string, 1)
	var requestRestart func(reason string) error
	if exeErr == nil {
		requestRestart = func(reason string) error {
			select {
			case restartCh <- reason:
			default: // 已经有一次在排队了，这一次直接算数
			}
			return nil
		}
	}

	rt, err := buildRuntime(cfg, runtimeOverrides{Restart: requestRestart})
	if err != nil {
		log.Fatalf("%v", err)
	}
	ag, store, plugins, models, cur := rt.agent, rt.store, rt.plugins, rt.models, rt.current

	auth, err := server.NewAuthStore(uiLang, cfg.BaseDir)
	if err != nil {
		log.Fatalf("加载访问口令失败: %v", err)
	}

	// 启动守卫：配了对外监听却没设口令时，降级为只监听回环而不是拒绝启动。
	// 拒绝启动在新装的远程机器上是个死局——口令要在界面里设，而界面起不来。
	// 降级同样一次都没暴露过，且 SSH 隧道进来就能把口令补上。
	exposed := !server.IsLoopbackHost(cfg.Server.Host)
	if exposed && !auth.HasPassword() {
		log.Printf("⚠ 配置的监听地址是 %s，但尚未设置访问口令，已降级为只监听 127.0.0.1。", cfg.Server.Host)
		log.Printf("  设置口令后重启即可对外提供服务：在本机执行 wen config server，")
		log.Printf("  或用 ssh -L %d:127.0.0.1:%d <用户>@<服务器> 建隧道后在设置页的「访问控制」里设置。",
			cfg.Server.Port, cfg.Server.Port)
		cfg.Server.Host = "127.0.0.1"
		exposed = false
	}

	srv := server.New(ag, store, plugins, models, server.Options{
		Auth:          auth,
		TrustLoopback: cfg.Server.TrustLoopbackOrDefault(),
		Exposed:       exposed,
		Lang:          uiLang,
	})
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)

	log.Printf("配置文件: %s", path)
	log.Printf("语言: %s", langSummary(cfg))
	log.Printf("模型: %s/%s  会话目录: %s", cur.ProviderName, cur.ModelID, cfg.SessionDir())
	for _, st := range plugins.List() {
		state := "禁用"
		if st.Enabled {
			state = "启用"
		}
		log.Printf("插件: %-12s [%s] %s", st.Name, state, st.Description)
	}
	log.Printf("访问控制: %s", authSummary(auth, exposed, cfg.Server.TrustLoopbackOrDefault()))
	log.Printf("Wen Agent %s 已启动: http://%s", version.Version, addr)

	// 登记运行中的实例，供 wen config 判定该走在线还是离线模式。
	// 失败只是让配置工具退回离线模式，不该拦住服务启动。
	if prev, ok := runlock.Read(cfg.BaseDir); ok && reachable(prev.Addr) {
		log.Printf("警告: 同一配置目录下已有实例在 %s 运行。两个实例共用一份状态文件会互相覆盖配置。", prev.Addr)
	}
	releaseLock, err := runlock.Acquire(cfg.BaseDir, runlock.Info{
		PID: os.Getpid(), Addr: addr, Version: version.Version,
		Started: time.Now().Format(time.RFC3339),
	})
	if err != nil {
		log.Printf("提示: 无法登记运行状态（%v），wen config 将只能离线修改配置", err)
	}
	defer releaseLock()

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 优雅退出：收到中断信号后先关 HTTP，再停掉插件的后台活动（定时器、长连接）
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	var restartReason string
	select {
	case <-rootCtx.Done():
		log.Printf("收到退出信号，正在关闭…")
	case restartReason = <-restartCh:
		log.Printf("%s：正在关闭当前实例…", restartReason)
	}
	// 先停插件、再关 HTTP：反过来的话，插件的后台轮次要等 HTTP 那边最多 5 秒的
	// 等待走完才被取消，于是「收到退出信号」之后还会有工具调用继续落盘——实际见过。
	// 这个顺序下正在处理的 HTTP 请求仍有 5 秒收尾时间，只是它们依赖的插件已经停了，
	// 而那正是退出时想要的：不再开始新的后台工作。
	plugins.StopAll()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutCtx)

	if restartReason != "" {
		// 交棒之前必须先松开这两样：端口（Windows 上新进程要重新 bind 它）与
		// 登记文件（新进程起来会重新登记）。顺序是硬的——Shutdown 已经关掉监听，
		// 这里补上登记文件，然后才轮到新进程。
		releaseLock()
		relaunch(exePath)
	}
}

// relaunch 用（已经被换成新版的）可执行文件接替当前进程。
//
// 两个平台的做法不同（见 updater.Relaunch）：Linux 与 macOS 上 execve 原地换掉进程
// 映像，PID 不变；Windows 没有这一层，只能起新进程再让自己退出。失败不是致命的——
// 程序文件已经是新版，人工启动一次就行，所以这里只报告，让日志说清现状。
func relaunch(exePath string) {
	if exePath == "" {
		log.Printf("无法定位当前程序，未能自动重启。程序文件已是新版，手工启动即可。")
		return
	}
	log.Printf("正在用新版程序重启：%s", exePath)
	if err := updater.Relaunch(exePath, os.Args[1:]); err != nil {
		log.Printf("自动重启失败: %v。程序文件已是新版，手工启动即可。", err)
		return
	}
	if updater.RelaunchExits {
		log.Printf("新版程序已启动，本进程退出。")
	}
}

// authSummary 把访问控制的实际状态写成一行，供启动日志使用。
// 「配了什么」和「实际生效什么」可能不一致（降级、反代关掉回环免认证），
// 所以这里说的是生效结果。
func authSummary(auth *server.AuthStore, exposed, trustLoopback bool) string {
	switch {
	case !auth.HasPassword():
		return "未设置口令，仅本机可访问"
	case !trustLoopback:
		return "已设置口令，所有来源（含本机）都需登录"
	case exposed:
		return "已设置口令，本机免登录、其它来源需登录"
	default:
		return "已设置口令，但当前只监听本机，不会被外部访问"
	}
}

// defaultOffPlugins 是默认不启用的插件。三类理由，各自成立：
//
//  1. 不配置就没法工作：不填触发词、城市、凭据，插件就是个空壳，默认打开只会让人
//     以为功能坏了。
//  2. 权限过大：read_file 能读任意文件、exec_command 能执行任意命令。这两样是
//     「拿它当干活的 Agent」时才要的东西，不该是开箱默认——尤其它们能绕过可见域
//     直接读到会话文件与记忆目录（dual_persona 正是为此把它们声明为冲突）。
//     要用就去设置页打开，那是个看得见的选择。
//  3. 空目录：skills 的技能目录是空的，开着只多两个用不上的工具。
//
// 其余插件用声明的默认参数就能直接工作，因此默认启用——**包括整个角色演绎组**。
// 它们从前全在这张表里，理由是「依赖默认关闭的 roleplay」，而 roleplay 关着的理由
// 是「没有角色设定就不成其为角色扮演」。那条理由成立，但结论下反了：该做的是给一个
// 默认角色设定（见 roleplay 的 defaultPersona），不是把这一组功能整个关掉。
//
// heartbeat 与 agenda 会在无人值守时消耗模型额度，这一点写在文档里而不是靠默认
// 关闭来表达：它们是这个项目的招牌功能（角色会主动开口、有自己的一天），关着的话
// 开箱就演示不出来。要停就去设置页关掉，或把心跳间隔调长。
var defaultOffPlugins = map[string]bool{
	"read_file":    true, // 能读任意文件
	"exec_command": true, // 能执行任意命令

	"dual_persona": true, // 不填触发词就永远切不过去
	"weather":      true, // 不填城市就查不了天气，且开箱即联网
	"skills":       true, // 技能目录是空的，开着只会多两个用不上的工具

	"qq_bot":       true, // 不填 AppID/AppSecret 没法工作
	"wechat_bot":   true, // 不扫码绑定没法工作
	"feishu_bot":   true, // 不填 App ID/Secret 没法工作
	"lark_bot":     true, // 同上
	"telegram_bot": true, // 不填 Bot Token 没法工作
}

// buildPlugins 注册全部内置系统插件，注册顺序即提示词拼接顺序。
//
// 开关与配置的唯一来源是 <配置目录>/plugins.state.json（由设置页维护）；这里给出的只是
// 首次安装、状态文件还不存在时的初值。
// ictx 中的模型与会话能力在 Agent 建好之前就要传进来，全部是闭包延迟取值。
func buildPlugins(cfg *config.Config, ictx plugin.InitContext, restart selfupdate.RestartFunc, opts ...plugin.Option) *plugin.Manager {
	m := plugin.NewManager(ictx, filepath.Join(cfg.BaseDir, "plugins.state.json"), opts...)
	builtins := builtinPlugins(ictx.Lang, cfg, restart)
	for _, p := range builtins {
		// Config 留空：插件自己声明的默认值就是初值，不需要在这里重复一遍
		init := plugin.PluginConfig{Enabled: !defaultOffPlugins[p.Name()]}
		if err := m.Register(p, init); err != nil {
			log.Printf("警告: 注册插件 %q 失败: %v", p.Name(), err)
		}
	}
	registerBundles(m, ictx.Lang, cfg.BaseDir)
	// 依赖校验必须等全部注册完：register 是逐个进行的，依赖方可能先于被依赖方注册
	m.Resolve()
	return m
}

// registerBundles 把已安装的外源插件包（Agent Plugins 1.0）注册进来，一个包一张卡片。
//
// 排在全部内置插件之后：它们不注入任何提示词，注册顺序对它们只剩展示含义。默认启用
// ——装一个包是用户特意做的动作，装完还要再去开一次开关是多余的一步；不想要就卸掉，
// 或在设置页上关掉（关掉的状态记在 plugins.state.json 里，与内置插件同一套）。
//
// 读不出来的包只记一行日志，不拦启动：一个坏掉的插件包不该让整个服务起不来。
func registerBundles(m *plugin.Manager, lang, baseDir string) {
	store := agentplugin.NewStore(lang, baseDir)
	bundles, problems := store.List()
	for _, p := range problems {
		log.Printf("警告: 插件包 %s", p)
	}
	for _, b := range bundles {
		p := agentplugin.NewPlugin(lang, b, store)
		if err := m.RegisterExternal(p, plugin.PluginConfig{Enabled: true}); err != nil {
			log.Printf("警告: 注册插件包 %q 失败: %v", b.Manifest.Name, err)
			continue
		}
		// 包自己的提醒（未知字段之类）不在这里打：buildPlugins 也被 wen config 与
		// wen plugins 调用，日志会糊在 TUI 上。它们由 wen plugins list / info 展示。
	}
}

// builtinPlugins 造出全部内置插件，顺序即注册顺序。
//
// 与 buildPlugins 分开，是为了让检查工具能拿到插件实例本身而不只是注册后的状态：
// 操作（Actions）只在插件启用且初始化之后才经 Manager 暴露出来，而漏译的检查不该
// 要求把消息通道、心跳与定时任务真的跑起来。
func builtinPlugins(lang string, cfg *config.Config, restart selfupdate.RestartFunc) []plugin.Plugin {
	// agenda 排「和谁」时只认人物库里的名字：people 的只读查询经构造函数交给它，
	// 这是插件之间直接的 Go 依赖，不走核心机制（见 people.Lookup 的说明）
	pp := people.New()
	return []plugin.Plugin{
		readfile.New(), execcmd.New(), webfetch.New(),
		// roleplay 必须在 dualpersona 之前：表人格设定要排在里人格设定前面，
		// 后者才能形成追加与覆盖的语义；scene 的舞台设定排在人格设定之后——先立角色，再立舞台，
		// weather 紧跟 scene：它讲的是舞台之外那个现实地方的天气，与舞台冲突时以舞台为准，
		// 这条规则得排在舞台设定之后才读得通；
		// belongings 排在舞台之后：持有物是舞台上的资产，先立角色与舞台才有归属对象；
		// body_sense 再排在其后：身体感知要有角色与场景在先，才有作用对象；
		// health 紧跟 body_sense、先于 mood：身体状况先于心情，心情的判据会提到身体；
		// people 排在心情之后：人物是角色与舞台立起来之后的关系网，agenda 紧随其后：日程是这群人之间的一天；
		// relationship 紧跟 people：对方是关系最近的那个人，先立人物再立关系；unspoken 紧跟其后：
		// 心里话是在这段关系里憋着的，关系先定，潜台词才有落点；
		// presence 收尾：现场快照是角色、舞台、身体、心情、关系都立起来之后「此刻」的定格，
		// 它的 [当下状态] 排在本轮状态块的最后，离生成位置最近；
		// style_watch 跟在 presence 之后：它不注入任何提示词，只观察轮次结束，放在
		// 角色演绎这组的末尾是为了设置页上与 mood、presence 同节展示
		roleplay.New(), dualpersona.New(), scene.New(), weather.New(), belongings.New(), bodysense.New(), health.New(), mood.New(), pp, agenda.New(pp), relationship.New(), unspoken.New(), presence.New(),
		stylewatch.New(),
		// memory 与 session_search 排在角色演绎那组之后。它们注入的是「什么该记下来」
		// 这类能力判据，与 scene / mood / weather 的判据同一类，挨在一起模型才会同等对待。
		// 早先它们排在最前面，落在 [角色设定 · 最高优先级] 声明之前，那句「以下设定优先于
		// 其它一般性指令」正好把记忆判据归进了被压过的那一类——实测同一批对话里
		// adjust_mood 与 record_touch 都被调用过，save_memory 一次也没有。
		// 顺带的可见后果：设置页上「角色演绎」这一节因此排到了「记忆与检索」之前。
		memory.New(), sessionsearch.New(),
		// skills 同理排在角色演绎那组之后：它注入的「遇到相符的任务先读手册」也是一条
		// 能力判据，落在 [角色设定 · 最高优先级] 之前会被那句「以下设定优先于其它一般性
		// 指令」一并压过去，成为可读可不读的建议。
		skills.New(),
		heartbeat.New(), scheduler.New(),
		// 飞书与 Lark 是同一套实现的两次实例化（见 larkbot 包注释）：同一套 API 的两个
		// 租户域，凭证互不通用，分成两个插件才能同时连两边
		qqbot.New(), wechatbot.New(),
		larkbot.NewFeishu(), larkbot.NewLark(), telegrambot.New(),
		// self_update 排在最后：它不注入任何提示词，注册顺序对它只剩展示含义，
		// 而「程序维护」这一节摆在设置页最下面也是对的——它与角色、记忆、通道都无关
		selfupdate.New(selfupdate.Options{Restart: restart}),
		// bundles 排在 self_update 旁边，理由相同：它不注入任何提示词，注册顺序对它
		// 只剩展示含义。它得排在下面 registerBundles 注册的那些包**之前**，设置页上
		// 「外源插件」这一节才会以「装一个新的」开头，而不是把入口埋在一堆卡片后面。
		bundles.New(lang, cfg.BaseDir),
	}
}
