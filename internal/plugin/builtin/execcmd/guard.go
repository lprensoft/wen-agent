package execcmd

import (
	"regexp"
	"strings"

	"wen/internal/prompt"
)

// 本文件是命令风险分级。
//
// 这里刻意**不做沙箱**。这个工具把任意字符串交给 cmd /C 或 sh -c，而 shell 有 &&、|、
// 变量展开、for /f、base64 解码、UNC 路径、..\.. 这些东西——想靠分析命令文本得出
// 「它会碰哪些文件」是做不到的。做一个能被轻易绕过的路径约束，比不做更糟：用户会信它。
// 真正的隔离要靠操作系统层面的机制（容器 / seccomp / AppContainer），那与「在真实工作
// 目录里跑 build、test、git」这个用途本身冲突。
//
// 真正能挡住误删的是「有人在 rm -rf 执行前看了一眼」。所以这里只做分级，把不可逆的
// 操作交给人判断：
//   - deny    完全没有正当用途、且影响是机器级不可逆的，直接拒绝，连问都不问；
//   - confirm 有正当用途但可能造成不可逆损失，交给用户确认；
//   - allow   放行。
//
// 局限要说清楚：模式匹配挡的是**误操作**（模型手滑、命令写错），挡不住刻意混淆
// （base64、变量拼接、自己写个脚本再执行）。要防后者只有 guardAll 一档——除只读白名单
// 外一律确认——那是唯一完整的设置。

// 拦截档位。
const (
	guardOff       = "off"       // 不拦截
	guardDangerous = "dangerous" // 只拦截判定为危险的命令（默认）
	guardAll       = "all"       // 除只读白名单外全部确认
	defaultGuard   = guardDangerous
)

// verdict 是对一条命令的判定。
type verdict int

const (
	verdictAllow verdict = iota
	verdictConfirm
	verdictDeny
)

// rule 是一条风险模式。
type rule struct {
	re *regexp.Regexp
	// risk 是这一类风险的名字。它既进工具返回给模型的错误文本，也进确认弹窗里那句
	// 「因为什么要确认」。两处都跟随进程语言，因此放提示词语言包——载重的那一半是
	// 模型那边，弹窗只是顺带同语言。
	risk prompt.Piece
}

func mk(risk prompt.Piece, patterns ...string) []rule {
	out := make([]rule, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, rule{re: regexp.MustCompile(p), risk: risk})
	}
	return out
}

// denyRules 是直接拒绝的模式：没有任何正当用途，且后果是整机级、不可撤销的。
// 这一档不问用户——把「格式化磁盘吗？」摆到人面前，本身就是一次误点击的机会。
var denyRules = concat(
	mk(prompt.Text("prompt.exec_command.risk.format_disk", "整盘格式化或写裸设备"),
		`(?i)\bformat\s+[a-z]:`,
		`(?i)\bmkfs(\.\w+)?\b`,
		`(?i)\bdd\b[^|;&]*\bof=/dev/(sd|nvme|hd|disk)`,
		`(?i)>\s*/dev/(sd|nvme|hd|disk)`,
		`(?i)\bdiskpart\b`,
	),
	mk(prompt.Text("prompt.exec_command.risk.delete_root", "删除系统根目录或整个用户目录"),
		`(?i)\brm\s+(-[a-z]*\s+)*-[a-z]*[rf][a-z]*\s+(-[a-z]+\s+)*/(\s|$|\*)`,
		`(?i)\brm\s+(-[a-z]*\s+)*-[a-z]*[rf][a-z]*\s+(-[a-z]+\s+)*~/?(\s|$|\*)`,
		`(?i)\b(del|erase|rd|rmdir)\b[^|;&]*\s[a-z]:\\?\s*$`,
		`(?i)\b(del|erase)\b[^|;&]*/s[^|;&]*\s[a-z]:\\(\s|$)`,
	),
	mk(prompt.Text("prompt.exec_command.risk.destroy_backup", "销毁备份或引导配置"),
		`(?i)\bvssadmin\b[^|;&]*\bdelete\b`,
		`(?i)\bwbadmin\b[^|;&]*\bdelete\b`,
		`(?i)\bbcdedit\b`,
		`(?i)\bcipher\b[^|;&]*/w`,
	),
	mk(prompt.Text("prompt.exec_command.risk.shutdown", "关机或重启"),
		`(?i)\bshutdown\b`, `(?i)\breboot\b`, `(?i)\bpoweroff\b`, `(?i)\bhalt\b`,
	),
	mk(prompt.Text("prompt.exec_command.risk.fork_bomb", "fork 炸弹"),
		`:\s*\(\s*\)\s*\{.*\|.*&.*\}\s*;?\s*:`,
	),
)

// dangerRules 是需要用户确认的模式：有正当用途，但做错了不可撤销。
var dangerRules = concat(
	mk(prompt.Text("prompt.exec_command.risk.delete_files", "删除文件或目录"),
		`(?i)(^|[\s;&|(])rm\s`,
		`(?i)(^|[\s;&|(])(del|erase|rd|rmdir)\s`,
		`(?i)\bRemove-Item\b`,
		`(?i)\b(shred|sdelete|truncate)\b`,
		`(?i)\bfind\b[^|;&]*(-delete|-exec\s+rm)`,
		`(?i)\bgit\s+clean\b`,
	),
	mk(prompt.Text("prompt.exec_command.risk.mirror_sync", "覆盖或镜像同步（会删除目标端多余文件）"),
		`(?i)\brobocopy\b[^|;&]*/(mir|purge)\b`,
		`(?i)\brsync\b[^|;&]*--delete`,
		`(?i)(^|[\s;&|(])(mv|move)\s[^|;&]*(-f|/y)\b`,
		`(?i)\bxcopy\b[^|;&]*/y\b`,
	),
	mk(prompt.Text("prompt.exec_command.risk.rewrite_history", "丢弃未提交改动或改写提交历史"),
		`(?i)\bgit\s+reset\b[^|;&]*--hard`,
		`(?i)\bgit\s+checkout\b[^|;&]*\s--\s`,
		`(?i)\bgit\s+restore\b`,
		`(?i)\bgit\s+push\b[^|;&]*(--force|-f)\b`,
		`(?i)\bgit\s+branch\b[^|;&]*\s-D\b`,
		`(?i)\bgit\s+(rebase|filter-branch|filter-repo)\b`,
		`(?i)\bgit\s+reflog\b[^|;&]*expire`,
		`(?i)\bgit\s+stash\b[^|;&]*(drop|clear)`,
	),
	mk(prompt.Text("prompt.exec_command.risk.change_perms", "修改权限或归属"),
		`(?i)(^|[\s;&|(])(chmod|chown|chgrp)\s`,
		`(?i)\b(icacls|takeown|attrib)\b`,
	),
	mk(prompt.Text("prompt.exec_command.risk.escalate", "提权执行"),
		`(?i)(^|[\s;&|(])(sudo|doas|runas)\s`,
		`(?i)(^|[\s;&|(])su\s+-`,
		`(?i)\bStart-Process\b[^|;&]*-Verb\s+RunAs`,
	),
	mk(prompt.Text("prompt.exec_command.risk.kill_process", "终止进程"),
		`(?i)(^|[\s;&|(])(kill|killall|pkill)\s`,
		`(?i)\b(taskkill|Stop-Process)\b`,
	),
	mk(prompt.Text("prompt.exec_command.risk.system_config", "改动系统服务、计划任务或注册表"),
		`(?i)\b(schtasks|systemctl|launchctl|netsh)\b`,
		`(?i)(^|[\s;&|(])sc\s+(create|delete|config|stop|start)\b`,
		`(?i)\breg\s+(add|delete|import)\b`,
		`(?i)\bsetx\b`,
		`(?i)\bHKLM:|\bHKEY_LOCAL_MACHINE\b`,
	),
	mk(prompt.Text("prompt.exec_command.risk.package_change", "安装或卸载软件包（会改动本机环境）"),
		`(?i)\b(apt|apt-get|yum|dnf|pacman|brew|choco|winget|scoop)\s+(install|remove|uninstall|upgrade|purge)`,
		`(?i)\bnpm\s+(install|i|uninstall|remove)\b[^|;&]*\s(-g|--global)\b`,
		`(?i)\b(pip|pip3)\s+(install|uninstall)\b`,
		`(?i)\bgo\s+install\b`,
	),
	mk(prompt.Text("prompt.exec_command.risk.pipe_to_shell", "从网络下载后直接执行"),
		`(?i)\b(curl|wget|iwr|Invoke-WebRequest)\b[^|;&]*\|\s*(sudo\s+)?(sh|bash|zsh|python\w*|pwsh|powershell)`,
		`(?i)\b(iex|Invoke-Expression)\b`,
	),
	mk(prompt.Text("prompt.exec_command.risk.network_write", "写入网络或落地文件"),
		`(?i)\bcurl\b[^|;&]*\s-(o|O)\b`,
		`(?i)\bcurl\b[^|;&]*-X\s*(POST|PUT|DELETE|PATCH)`,
		`(?i)\bwget\b[^|;&]*\s-O\b`,
		`(?i)\bInvoke-WebRequest\b[^|;&]*-OutFile`,
	),
	mk(prompt.Text("prompt.exec_command.risk.delete_container", "删除容器、镜像或数据卷"),
		`(?i)\bdocker\b[^|;&]*\b(rm|rmi|prune)\b`,
		`(?i)\bdocker\s+volume\s+rm\b`,
		`(?i)\bkubectl\s+delete\b`,
	),
	mk(prompt.Text("prompt.exec_command.risk.delete_db", "删除数据库或数据表"),
		`(?i)\bdrop\s+(table|database|schema)\b`,
		`(?i)\btruncate\s+table\b`,
		`(?i)\bdelete\s+from\b`,
	),
)

// readOnlyPrefixes 是只读白名单，仅在 guardAll 档位下用来免去确认。
// 只匹配命令的开头若干词，且要求整条命令不含任何串联、重定向与命令替换——
// 一旦出现那些，就不能再从开头几个词推断这条命令到底做了什么。
var readOnlyPrefixes = []string{
	"git status", "git log", "git diff", "git show", "git blame", "git remote -v",
	"git rev-parse", "git describe", "git ls-files", "git stash list", "git branch",
	"go build", "go test", "go vet", "go list", "go version", "go env", "go doc",
	"cargo check", "cargo build", "cargo test",
	"npm ls", "npm run test", "node --version", "python --version", "python3 --version",
	"ls", "dir", "pwd", "cd", "cat", "type", "head", "tail", "wc",
	"findstr", "grep", "rg", "fd", "tree", "where", "which", "whoami", "hostname",
	"echo", "date", "env", "printenv", "df", "du", "ps", "top",
	"stat", "file", "diff", "cmp", "md5sum", "sha256sum", "certutil -hashfile",
}

// shellMeta 是会让「从开头几个词判断意图」失效的记号。
var shellMeta = regexp.MustCompile("[|;&><`$]|\\$\\(|\n|\r")

// segmentSep 按 shell 的串联记号切分命令。危险模式可能藏在后半段
// （`go build && rm -rf out`），逐段检查才不会漏。
var segmentSep = regexp.MustCompile(`\|\||&&|[|;&\n\r]`)

// classify 判定一条命令的风险，返回判定与原因。
func classify(command, guard, lang string) (verdict, string) {
	cmd := strings.TrimSpace(command)
	if cmd == "" || guard == guardOff {
		return verdictAllow, ""
	}

	// 既查整条、也逐段查。逐段是为了不漏掉藏在后半段的危险模式
	// （`go build && rm -rf out`，段内的 ^ 锚点只有分段后才对得上）；
	// 查整条是为了不漏掉本身就跨越串联记号的模式（管道进 shell、fork 炸弹）。
	targets := []string{cmd}
	for _, seg := range segmentSep.Split(cmd, -1) {
		if seg = strings.TrimSpace(seg); seg != "" {
			targets = append(targets, seg)
		}
	}
	sep := prompt.T(lang, "prompt.exec_command.risk.sep", "；")
	if reasons := matchAll(denyRules, targets, lang); len(reasons) > 0 {
		return verdictDeny, strings.Join(reasons, sep)
	}
	// 列出全部命中的类别而不是只报第一条：一条串联命令可能既删文件又改写提交历史，
	// 只说一样会让人以为自己已经看清了要同意什么。
	if reasons := matchAll(dangerRules, targets, lang); len(reasons) > 0 {
		return verdictConfirm, strings.Join(reasons, sep)
	}

	if guard == guardAll && !isReadOnly(cmd) {
		return verdictConfirm, prompt.T(lang, "prompt.exec_command.risk.not_read_only", "未在只读白名单中")
	}
	return verdictAllow, ""
}

// matchAll 返回命中的全部风险类别，按规则声明顺序去重。
func matchAll(rules []rule, targets []string, lang string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rules {
		if seen[r.risk.Key()] {
			continue
		}
		for _, s := range targets {
			if r.re.MatchString(s) {
				seen[r.risk.Key()] = true
				out = append(out, r.risk.For(lang))
				break
			}
		}
	}
	return out
}

// isReadOnly 判断命令是否落在只读白名单里。
func isReadOnly(cmd string) bool {
	if shellMeta.MatchString(cmd) {
		return false // 含串联或重定向，开头几个词说明不了这条命令做什么
	}
	lower := strings.ToLower(strings.Join(strings.Fields(cmd), " "))
	for _, p := range readOnlyPrefixes {
		if lower == p || strings.HasPrefix(lower, p+" ") {
			return true
		}
	}
	return false
}

func concat(groups ...[]rule) []rule {
	var out []rule
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}
