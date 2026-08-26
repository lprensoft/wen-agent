package updater

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"wen/internal/i18n"
)

// workDirName 是下载与解压的落脚点，建在**可执行文件所在的目录**里。
//
// 不用系统临时目录有两个原因：换上去那一步靠 rename，跨设备的 rename 会失败；
// 而 Windows 上的杀软对「临时目录里的 exe」格外敏感——这个项目已经在那上面栽过一次。
const workDirName = ".wen-update"

// smokeTimeout 是试运行新二进制的时限。它只是打印一行版本号，慢到这个地步说明有问题
// （比如正被杀软拦着）。
const smokeTimeout = 30 * time.Second

// ExePath 返回当前可执行文件的真实路径（跟随符号链接）。
//
// 跟随符号链接是必要的：Linux 上常见把二进制放一处、在 PATH 里挂个链接，
// 直接盖掉链接会把安装布局改掉，而用户以为只是升了个级。
func ExePath(lang string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf(i18n.T(lang, "updater.err.exe_path", "无法定位当前程序: %w"), err)
	}
	// Linux 上这个值来自 /proc/self/exe，文件被替换之后它会变成「/路径/wen (deleted)」。
	// 自更新做的正是替换这个文件，于是更新之后再问一次就会拿到一个不存在的路径。
	exe = strings.TrimSuffix(exe, " (deleted)")
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// HostBinaryName 是本机平台上那个可执行文件的名字。
func HostBinaryName() string { return BinaryName(runtime.GOOS) }

// WorkDir 是这次更新的工作目录。
func WorkDir(exe string) string { return filepath.Join(filepath.Dir(exe), workDirName) }

// backupPath 是替换时旧文件的去处。
//
// 用 `wen.exe.old` 而不是 `wen.old.exe`：Windows 上留下的残留文件不该长得像个
// 能双击的程序。
func backupPath(exe string) string { return exe + ".old" }

// freeBackupPath 找一个当前没被占用的旧文件去处。
//
// 常规情况下就是 `wen.exe.old`。它可能还在：Windows 上运行中的映像删不掉，
// 而「关掉自动重启、连更新两次」正好会走到那里——上一次的旧映像仍是当前进程，
// 删不掉也不能覆盖。那时往后顺延一个编号，全部由 CleanupOld 在下次启动时一并清掉。
func freeBackupPath(exe string) string {
	base := backupPath(exe)
	for i := 0; i < 10; i++ {
		p := base
		if i > 0 {
			p = fmt.Sprintf("%s.%d", base, i)
		}
		if err := os.Remove(p); err == nil || os.IsNotExist(err) {
			return p
		}
	}
	return base // 十个都占着的话按原样来，让 Rename 去报那个错
}

// CheckWritable 在下载之前确认安装目录可写。
//
// 提前查是为了把最常见的那种失败说清楚：装在 /usr/local/bin、Program Files 或
// 由包管理器装的，目录本来就不归当前用户写。等下完十几兆再报错，白花时间也白花流量。
func CheckWritable(lang, exe string) error {
	dir := filepath.Dir(exe)
	f, err := os.CreateTemp(dir, ".wen-write-check-*")
	if err != nil {
		return fmt.Errorf(i18n.T(lang, "updater.err.not_writable",
			"安装目录 %s 不可写（%v）。程序可能是由包管理器或管理员安装的，"+
				"这种情况请用原来的方式升级，或手工下载新版替换"), dir, err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

// SmokeTest 跑一次新下载的二进制，确认它在这台机器上真的能起来、且确实是目标版本。
//
// 这一步挡住的是「校验和没问题但根本跑不了」：架构不符、被杀软改写或隔离、
// 文件系统挂了 noexec。它在替换之前做，失败就当作这次更新没发生过。
func SmokeTest(ctx context.Context, lang, bin, wantTag string) error {
	ctx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf(i18n.T(lang, "updater.err.smoke_run", "新版程序无法运行（%v）。可能被杀毒软件拦截或隔离，本次更新已取消"), err)
	}
	got := strings.TrimSpace(string(out))
	if got != wantTag {
		return fmt.Errorf(i18n.T(lang, "updater.err.smoke_version", "新版程序自报版本为 %q，与预期的 %s 不符，本次更新已取消"), got, wantTag)
	}
	return nil
}

// CleanupOld 清掉上一次更新留下的旧文件。
//
// 只在启动时调用：Windows 上运行中的映像删不掉，那次更新只能把它改名留在原地，
// 到下次启动才轮得到它。删不掉就算了——它只是块占地方的垃圾，不影响任何功能。
func CleanupOld(exe string) {
	matches, _ := filepath.Glob(backupPath(exe) + "*")
	for _, m := range matches {
		os.Remove(m)
	}
	os.RemoveAll(WorkDir(exe))
}
