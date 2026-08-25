package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"

	"wen/internal/config"
)

// runConfig 是引导式配置的入口。
//
// 分在线与离线两种模式（见 backend）：服务在跑时经它的接口改，改动立即生效；
// 没在跑时直接改配置文件。模式在每一屏的顶部标明——改动什么时候生效是用户必须
// 知道的事，不该让人自己去判断。
func runConfig(args []string) error {
	section := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		section, args = args[0], args[1:]
	}
	switch section {
	case "", "plugins", "models", "server":
	default:
		return fmt.Errorf("未知的配置栏目 %q（可用：plugins、models、server）", section)
	}

	fs := flag.NewFlagSet("config", flag.ExitOnError)
	configPath := fs.String("c", "", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）")
	_ = fs.Parse(args)

	if !isInteractive() {
		return errors.New("wen config 需要在终端里运行（当前的标准输入不是终端）")
	}

	path := config.ResolvePath(*configPath)
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	adoptLang(cfg)
	b, err := openBackend(cfg)
	if err != nil {
		return err
	}

	fmt.Printf("配置文件 %s\n模式 %s\n\n", path, b.mode())

	// 直接指定栏目时先进那一屏，返回后落到顶层菜单，方便接着改别的
	for {
		if section == "" {
			next, err := topMenu(b)
			if err != nil {
				return err
			}
			if next == "quit" {
				return nil
			}
			// 选完语言回到顶层菜单：下一次渲染取的就是新语言的字符串，
			// 插件那一屏也会带着新语言重新向服务端要一次。
			if next == "language" {
				if err := languageMenu(); err != nil && !errors.Is(err, huh.ErrUserAborted) {
					return err
				}
				continue
			}
			section = next
		}
		var err error
		switch section {
		case "plugins":
			err = pluginsSection(b)
		case "models":
			err = modelsSection(b)
		case "server":
			err = accessSection(b)
		}
		if err != nil && !errors.Is(err, huh.ErrUserAborted) {
			return err
		}
		section = ""
	}
}

// isInteractive 判断标准输入输出是不是终端。
//
// 不能用「是否字符设备」来判断：Windows 上 /dev/null 映射到 NUL，而 NUL 正是个
// 字符设备，那样判会放行并让界面挂在那里等一个永远不来的按键。用 term.IsTerminal
// 才问到了真正的问题——这个句柄背后有没有控制台。
func isInteractive() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

func topMenu(b backend) (string, error) {
	// 初值决定光标停在哪一项（huh 会把光标定位到与当前值相同的选项）。
	// 用「退出」当初值等于每次进来光标都压在退出上，第一项才是想选的那个。
	choice := "plugins"
	err := run(huh.NewSelect[string]().
		Title(tr("cli.top.title", "要配置什么？")).
		Description(fitLines(b.mode())).
		Options(
			// 语言排第一项：它是「我看不懂这个界面」时的逃生口，逃生口不放在最后。
			huh.NewOption(languageEntry(), "language"),
			huh.NewOption(tr("cli.top.plugins", "插件 —— 开关与参数"), "plugins"),
			huh.NewOption(tr("cli.top.models", "模型 —— 提供商、密钥与当前模型"), "models"),
			huh.NewOption(tr("cli.top.server", "访问控制 —— 远程访问口令"), "server"),
			huh.NewOption(tr("cli.top.quit", "退出"), "quit"),
		).
		Value(&choice))
	if errors.Is(err, huh.ErrUserAborted) {
		return "quit", nil
	}
	return choice, err
}

// run 把单个字段包成一次表单运行。huh 的字段必须挂在 Form 上才能交互。
func run(f huh.Field) error {
	return huh.NewForm(huh.NewGroup(f)).Run()
}

// back 是各级列表里统一的返回项取值。
const back = "\x00back"

// note 打印一条操作结果。用 stdout 而不是 huh 的提示框，
// 是为了让它留在滚动区里——连续改多项时，前几项的结果还看得见。
func note(format string, a ...any) {
	fmt.Printf("  "+format+"\n", a...)
}
