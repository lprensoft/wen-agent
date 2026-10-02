package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"wen/internal/version"
)

// command 是一条子命令。
//
// 这张表是命令面的唯一来源：帮助文本、未知命令的提示、参数校验的用法说明全部由它
// 生成。手写一份帮助文本迟早会和实际支持的命令分叉，而分叉的帮助比没有帮助更糟。
type command struct {
	name    string
	usage   string
	summary string
	// detail 是展开的说明，只在 wen help <命令> 时显示；为空则不显示。
	detail string
	run    func(args []string) error
}

func commands() []command {
	return []command{
		{
			name:    "serve",
			usage:   tr("cli.cmd.serve.usage", "wen serve [-c 配置文件] [-p 端口]"),
			summary: tr("cli.cmd.serve.summary", "启动服务（不带子命令时的默认动作）"),
			detail: tr("cli.cmd.serve.detail", "读取配置文件、加载插件并监听 HTTP。\n"+
				"未设置访问口令时只监听本机，即便配置里写的是对外地址——\n"+
				"设置口令见 wen config server。"),
			run: func(args []string) error { runServe(args); return nil },
		},
		{
			name:    "onboard",
			usage:   tr("cli.cmd.onboard.usage", "wen onboard [-c 配置文件]"),
			summary: tr("cli.cmd.onboard.summary", "首次配置向导：选模型提供商与模型"),
			detail: tr("cli.cmd.onboard.detail", "第一次运行 wen 且还没有可用的模型时会自动进入，也可以随时手动重跑。\n"+
				"只问模型提供商、API Key 与模型，配置目录里还没有 config.yaml 时再问一句端口；\n"+
				"其余全部使用默认值。重跑时会新增一家提供商并切换过去，已有的不会动。"),
			run: runOnboard,
		},
		{
			name:    "config",
			usage:   tr("cli.cmd.config.usage", "wen config [plugins|models|server] [-c 配置文件]"),
			summary: tr("cli.cmd.config.summary", "引导式修改配置"),
			detail: tr("cli.cmd.config.detail", "不带参数时进入顶层菜单，可在各栏目之间来回切换。\n"+
				"服务正在运行时经它的接口修改，改动立即生效；\n"+
				"服务没在运行时直接改配置文件，下次启动生效。\n"+
				"两种模式会在界面上标明，不必自己判断。"),
			run: runConfig,
		},
		{
			name:    "eval",
			usage:   tr("cli.cmd.eval.usage", "wen eval <脚本.json> [-c 配置文件] [-o 报告文件]"),
			summary: tr("cli.cmd.eval.summary", "回放一段脚本对话，生成文风与角色一致性报告"),
			detail: tr("cli.cmd.eval.detail", "按脚本逐轮对话（可在中途强制压缩），用与 serve 相同的配置与插件，\n"+
				"但会话与插件数据全部落在临时目录，不碰真实数据；消息通道与后台任务类\n"+
				"插件不启动。跑完后统计每轮的助手腔命中与字数，并请模型按「像不像同一个人 /\n"+
				"语气是否一致 / 关系与称呼是否连续」三项各打 1-5 分。报告是 Markdown，\n"+
				"默认打到标准输出，-o 写入文件。示例脚本见 docs/eval/example.json。"),
			run: runEval,
		},
		{
			name:    "plugins",
			usage:   tr("cli.cmd.plugins.usage", "wen plugins [list|install|uninstall|info] [-c 配置文件]"),
			summary: tr("cli.cmd.plugins.summary", "管理外源插件包"),
			detail: tr("cli.cmd.plugins.detail", "装的是 Agent Plugins 1.0 格式的插件包：一个目录，里面有 plugin.json，\n"+
				"可带 skills/（技能）与 mcp.json（MCP server 声明）。\n"+
				"install 认本地目录与 .zip；装好的包在设置页上各占一张卡片，标记为「外源」。\n"+
				"插件在启动期注册，装完或卸完要重启服务才生效。\n"+
				"本版只加载技能，包里的 MCP 声明尚未支持。"),
			run: runPlugins,
		},
		{
			name:    "status",
			usage:   tr("cli.cmd.status.usage", "wen status [-c 配置文件]"),
			summary: tr("cli.cmd.status.summary", "打印当前配置与运行状态"),
			run:     runStatus,
		},
		{
			name:    "update",
			usage:   tr("cli.cmd.update.usage", "wen update [--check] [-y] [-c 配置文件]"),
			summary: tr("cli.cmd.update.summary", "检查并安装新版本"),
			detail: tr("cli.cmd.update.detail", "查 GitHub 上最新的正式版，把更新说明打出来。\n"+
				"加 -y 才会真的下载、校验并替换当前程序文件；--check 只查不装。\n"+
				"它不重启正在运行的服务——要一步到位地更新并重启，\n"+
				"用设置页「程序维护 · 程序更新」里的那个按钮。"),
			run: runUpdate,
		},
		{
			name:    "version",
			usage:   "wen version",
			summary: tr("cli.cmd.version.summary", "打印版本号"),
			run: func([]string) error {
				fmt.Println(version.Version)
				return nil
			},
		},
		{
			name:    "help",
			usage:   tr("cli.cmd.help.usage", "wen help [命令]"),
			summary: tr("cli.cmd.help.summary", "显示帮助"),
			run:     runHelp,
		},
	}
}

func lookup(name string) *command {
	for _, c := range commands() {
		if c.name == name {
			return &c
		}
	}
	return nil
}

func main() {
	args := os.Args[1:]

	// 帮助与未知命令的提示在读到配置之前就要打出来，先按系统区域定一门语言；
	// 子命令读到配置后会用 adoptLang 覆盖掉它。
	adoptLangEarly(args)

	// 分发规则：第一个参数不以 - 开头且命中命令表时走子命令，否则一律当作 serve
	// 的参数。这样 wen、wen -p 8080 的行为与加子命令之前完全一致。
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd := lookup(args[0])
		if cmd == nil {
			fmt.Fprint(os.Stderr, trf("cli.unknown_command", "未知命令 %q\n\n", args[0]))
			printHelp(os.Stderr)
			os.Exit(2)
		}
		if err := cmd.run(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		printHelp(os.Stdout)
		return
	}

	runServe(args)
}

func runHelp(args []string) error {
	if len(args) == 0 {
		printHelp(os.Stdout)
		return nil
	}
	cmd := lookup(args[0])
	if cmd == nil {
		return fmt.Errorf(tr("cli.unknown_command_err", "未知命令 %q"), args[0])
	}
	fmt.Print(trf("cli.help.usage_line", "用法: %s\n\n%s\n", cmd.usage, cmd.summary))
	if cmd.detail != "" {
		fmt.Printf("\n%s\n", cmd.detail)
	}
	return nil
}

func printHelp(w *os.File) {
	fmt.Fprint(w, trf("cli.help.head", "Wen Agent %s\n\n用法: wen [命令] [参数]\n\n命令:\n", version.Version))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range commands() {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	_ = tw.Flush()
	fmt.Fprint(w, tr("cli.help.tail", "\n不带命令时启动服务。wen help <命令> 查看单条命令的说明。\n"))
}
