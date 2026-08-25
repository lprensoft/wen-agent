package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wen/internal/plugin"
	"wen/internal/skillroots"
)

// 外源插件包带进来的技能与用户自己写的技能出现在同一份清单里，共用同一个
// read_skill——每个包再各自开一个工具的话，装三个包就会多出三个同名工具。
func TestBundleSkillsJoinTheSameList(t *testing.T) {
	skillroots.Reset()
	t.Cleanup(skillroots.Reset)

	own := t.TempDir()
	writeSkill(t, own, "my-note", "name: my-note\ndescription: 我自己写的", "正文")
	p := newAt(t, own, nil)

	if got := p.SystemPrompt(); strings.Contains(got, "from-pkg") {
		t.Fatal("还没登记就出现了包里的技能")
	}

	pkg := t.TempDir()
	writeSkill(t, pkg, "from-pkg", "name: from-pkg\ndescription: 包里带的", "正文")
	skillroots.Set(skillroots.Root{Owner: "ext_demo", Dir: pkg, Label: "demo"})

	got := p.SystemPrompt()
	if !strings.Contains(got, "my-note") || !strings.Contains(got, "from-pkg") {
		t.Fatalf("两处技能没有合到一份清单里:\n%s", got)
	}

	// 工具也要能读到包里的那一篇，否则清单列了却读不出来
	if _, ok := p.index["from-pkg"]; !ok {
		t.Fatal("包里的技能没进索引")
	}

	// 禁用插件包（Stop 摘掉登记）之后，它的技能要随即从清单里消失
	skillroots.Clear("ext_demo")
	if got := p.SystemPrompt(); strings.Contains(got, "from-pkg") {
		t.Fatalf("摘掉登记之后技能还在:\n%s", got)
	}
}

// 重名时用户自己的赢：那是他直接控制的东西，装进来的包不该悄悄盖掉它。
// 也不能两个都收——read_skill 按名字找，收下两个同名的等于让它随机命中一个。
func TestBundleSkillNameCollisionKeepsOwn(t *testing.T) {
	skillroots.Reset()
	t.Cleanup(skillroots.Reset)

	own := t.TempDir()
	writeSkill(t, own, "shared", "name: shared\ndescription: 用户自己的那份", "正文")
	pkg := t.TempDir()
	writeSkill(t, pkg, "shared", "name: shared\ndescription: 包里那份", "正文")
	skillroots.Set(skillroots.Root{Owner: "ext_demo", Dir: pkg, Label: "demo"})

	p := newAt(t, own, nil)
	if len(p.list) != 1 {
		t.Fatalf("同名技能该只留一个，实际 %d 个", len(p.list))
	}
	if !strings.Contains(p.list[0].Desc, "用户自己的") {
		t.Errorf("留下的不是用户自己那份: %q", p.list[0].Desc)
	}
	if len(p.problems) == 0 {
		t.Error("跳过一个同名技能之后该报一句，不能静默丢掉")
	}
}

// 技能目录经符号链接指到技能根之外时要拒绝，并且**要说一句**。
//
// 「说一句」是这条用例的重点：这里原先只判 os.ReadDir 给出的 IsDir，而它来自 lstat
// ——指向目录的符号链接在它眼里不是目录，于是整个条目在容纳性校验之前就被丢掉了，
// 表现为技能凭空消失、屏幕上一句解释都没有。
func TestSkillDirEscapingRootIsRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "skills")
	outside := filepath.Join(base, "outside")
	writeSkill(t, outside, "sneaky", "name: sneaky\ndescription: 外面的", "正文")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir(t, filepath.Join(outside, "sneaky"), filepath.Join(root, "sneaky"))

	res := scan(root, 120)
	if len(res.skills) != 0 {
		t.Fatalf("逃出技能根的目录被收下了: %+v", res.skills)
	}
	if len(res.problems) != 1 {
		t.Fatalf("拒绝之后该说一句，实际 %d 句: %v", len(res.problems), res.problems)
	}
	if !strings.Contains(res.problems[0], "sneaky") {
		t.Errorf("那句话该点名是哪个: %q", res.problems[0])
	}
}

// 指到技能根**之内**的符号链接照常收下：那是用户在自己目录里做的整理，没有理由拦。
func TestSkillDirLinkedWithinRootIsKept(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "real", "name: real\ndescription: 真身", "正文")
	linkDir(t, filepath.Join(root, "real"), filepath.Join(root, "alias"))

	res := scan(root, 120)
	if len(res.skills) != 2 {
		t.Fatalf("根内的链接该照常收下，实际收到 %d 个: %+v", len(res.skills), res.skills)
	}
	// 链接过来的那份 SKILL.md 里写的还是真身的名字，于是有一句「与目录名不一致」的
	// 提醒——那是对的，链接的两端本来就是两个名字。要保证的是没有「越界」那一句。
	for _, p := range res.problems {
		if strings.Contains(p, "指到了技能目录之外") {
			t.Errorf("根内的链接被当成越界拒掉了: %q", p)
		}
	}
}

// 插件包的开关走的是插件系统那一套：启用即登记，禁用即摘掉。
func TestBundlePluginContract(t *testing.T) {
	var _ plugin.Stoppable = (*Plugin)(nil)
}

// linkDir 建一条指向目录的链接。
//
// Windows 上普通用户建不了符号链接（要「创建符号链接」这项权限），但建得了目录联接
// （junction），而 Go 的 Lstat 同样把它报成符号链接——两条用例因此在 Windows 上也能
// 真的跑起来。这不是凑数：这个缺陷正是因为它们在开发机上一直 SKIP、只在 CI 上才跑，
// 才带着一句错误的断言进了库。
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS != "windows" {
		t.Skip("这个环境建不了指向目录的链接")
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("这个环境建不了目录联接：%v（%s）", err, out)
	}
}
