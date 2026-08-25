package skills

import (
	"os"
	"path/filepath"
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

// 技能目录不得经符号链接指到根之外。用户自己的目录里这多半是手滑，但插件包是从
// 外面拿来的，Agent Plugins 标准明确要求客户端拒绝这种逃逸。
func TestSkillDirEscapingRootIsRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "skills")
	outside := filepath.Join(base, "outside")
	writeSkill(t, outside, "sneaky", "name: sneaky\ndescription: 外面的", "正文")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "sneaky"), filepath.Join(root, "sneaky")); err != nil {
		t.Skipf("这个环境建不了符号链接：%v", err) // Windows 上非管理员建不了
	}

	res := scan(root, 120)
	if len(res.skills) != 0 {
		t.Fatalf("逃出技能根的目录被收下了: %+v", res.skills)
	}
	if len(res.problems) == 0 {
		t.Error("拒绝之后该说一句")
	}
}

// 插件包的开关走的是插件系统那一套：启用即登记，禁用即摘掉。
func TestBundlePluginContract(t *testing.T) {
	var _ plugin.Stoppable = (*Plugin)(nil)
}
