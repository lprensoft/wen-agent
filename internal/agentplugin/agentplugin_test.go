package agentplugin

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodManifest = `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "pdf-tools",
  "version": "1.2.0",
  "description": "Work with PDF files"
}`

func TestParseManifestGood(t *testing.T) {
	m, warnings, err := ParseManifest("", []byte(goodManifest))
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("不该有提醒: %v", warnings)
	}
	if m.Name != "pdf-tools" || m.Version != "1.2.0" {
		t.Fatalf("解析结果不对: %+v", m)
	}
}

// 闭合 schema 的「闭合」指的是顶层只有那几个字段，不是「见到别的就报废」：
// 标准要求未知字段报告并忽略，插件包仍然有效。用 1.0 的客户端读 1.1 写的包时，
// 多出来的字段不该让包装不上。
func TestParseManifestUnknownFieldIsWarningNotError(t *testing.T) {
	src := `{
	  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
	  "name": "x",
	  "futureThing": {"a": 1}
	}`
	m, warnings, err := ParseManifest("", []byte(src))
	if err != nil {
		t.Fatalf("未知字段不该让插件包报废: %v", err)
	}
	if m.Name != "x" {
		t.Fatalf("名字没读出来: %+v", m)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "futureThing") {
		t.Fatalf("该报告一次未知字段，实际: %v", warnings)
	}
}

func TestParseManifestRejects(t *testing.T) {
	cases := map[string]string{
		"缺 $schema": `{"name": "x"}`,
		"schema 版本不认识": `{"$schema": "https://agent-plugins.org/schemas/9.9.9/plugin.schema.json",
			"name": "x"}`,
		"缺 name": `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"}`,
		"名字有大写": `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
			"name": "PDF"}`,
		"名字连字符开头": `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
			"name": "-x"}`,
		"名字有连续连字符": `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
			"name": "a--b"}`,
		"不是 JSON": `not json at all`,
	}
	for name, src := range cases {
		if _, _, err := ParseManifest("", []byte(src)); err == nil {
			t.Errorf("%s：应当拒绝但通过了", name)
		}
	}
}

// extensions 是各客户端的私有命名空间，本程序一个都不实现。它写成什么样都不该
// 影响加载——标准明确要求不认识的命名空间跳过且不校验内容。
func TestParseManifestExtensionsNotObject(t *testing.T) {
	src := `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		"name": "x", "extensions": 12}`
	_, warnings, err := ParseManifest("", []byte(src))
	if err != nil {
		t.Fatalf("extensions 不是对象不该让插件包报废: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("该报告一句")
	}
}

func TestPluginName(t *testing.T) {
	cases := map[string]string{
		"pdf-tools": "ext_pdf_tools",
		"a.b.c":     "ext_a_b_c",
		"plain":     "ext_plain",
		"memory":    "ext_memory", // 与内置的 memory 不撞名，正是加前缀的理由
		"x-y.z":     "ext_x_y_z",
	}
	for in, want := range cases {
		if got := PluginName(in); got != want {
			t.Errorf("PluginName(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// writeBundle 在 dir 下造一个插件包，返回它的路径。
func writeBundle(t *testing.T, dir, manifest string, skills map[string]string, mcp string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range skills {
		sd := filepath.Join(dir, SkillsDirName, name)
		if err := os.MkdirAll(sd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if mcp != "" {
		if err := os.WriteFile(filepath.Join(dir, MCPFile), []byte(mcp), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadComponents(t *testing.T) {
	dir := writeBundle(t, filepath.Join(t.TempDir(), "pkg"), goodManifest,
		map[string]string{"summarize": "---\nname: summarize\ndescription: 摘要\n---\n正文"},
		`{"mcpServers": {}}`)

	b, err := Load("", dir)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if b.SkillsDir == "" {
		t.Error("没认出技能目录")
	}
	if !b.HasMCP {
		t.Error("没认出 mcp.json")
	}
}

func TestLoadWithoutManifest(t *testing.T) {
	dir := writeBundle(t, filepath.Join(t.TempDir(), "pkg"), "", nil, "")
	if _, err := Load("", dir); err == nil {
		t.Fatal("没有 plugin.json 的目录不该被当成插件包")
	}
}

func TestInstallFromDir(t *testing.T) {
	base := t.TempDir()
	src := writeBundle(t, filepath.Join(base, "src"), goodManifest,
		map[string]string{"a": "---\nname: a\ndescription: d\n---\n"}, "")

	store := NewStore("", filepath.Join(base, "cfg"))
	b, err := store.Install(src)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if b.Manifest.Name != "pdf-tools" {
		t.Fatalf("装错了: %+v", b.Manifest)
	}
	if _, err := os.Stat(filepath.Join(store.Path("pdf-tools"), ManifestFile)); err != nil {
		t.Fatalf("清单没复制过去: %v", err)
	}

	// 重复安装必须拒绝，而不是悄悄覆盖掉用户已经在用的那一份
	if _, err := store.Install(src); err == nil {
		t.Error("重复安装应当被拒绝")
	}

	list, problems := store.List()
	if len(list) != 1 || len(problems) != 0 {
		t.Fatalf("列表不对: %d 个包，%v", len(list), problems)
	}
	if err := store.Remove("pdf-tools"); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if list, _ := store.List(); len(list) != 0 {
		t.Error("卸载之后还在")
	}
}

// 装坏包不能留下残骸：先在原地校验、再复制，失败时存放目录一个字节都没动过。
func TestInstallBadBundleLeavesNothing(t *testing.T) {
	base := t.TempDir()
	src := writeBundle(t, filepath.Join(base, "src"), `{"name": "x"}`, nil, "")
	store := NewStore("", filepath.Join(base, "cfg"))
	if _, err := store.Install(src); err == nil {
		t.Fatal("清单不合法却装成功了")
	}
	if list, problems := store.List(); len(list) != 0 || len(problems) != 0 {
		t.Fatalf("失败的安装留下了残骸: %d 个包，%v", len(list), problems)
	}
}

func TestInstallFromZip(t *testing.T) {
	base := t.TempDir()
	zipPath := filepath.Join(base, "pkg.zip")
	writeZip(t, zipPath, map[string]string{
		// 顶层套一层目录，正是从 GitHub 下载的 zip 的样子
		"pdf-tools-main/plugin.json":       goodManifest,
		"pdf-tools-main/skills/a/SKILL.md": "---\nname: a\ndescription: d\n---\n",
	})
	store := NewStore("", filepath.Join(base, "cfg"))
	b, err := store.Install(zipPath)
	if err != nil {
		t.Fatalf("从 zip 安装失败: %v", err)
	}
	if b.SkillsDir == "" {
		t.Error("zip 里的技能目录没认出来")
	}
	// 解包用的临时目录不能留下
	entries, _ := os.ReadDir(store.Dir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".unpack-") {
			t.Errorf("解包临时目录没清掉: %s", e.Name())
		}
	}
}

// 压缩包里的名字是外面给的，写成 ../ 就能落到解压目录之外。
func TestInstallZipSlipRejected(t *testing.T) {
	base := t.TempDir()
	zipPath := filepath.Join(base, "evil.zip")
	writeZip(t, zipPath, map[string]string{
		"plugin.json":    goodManifest,
		"../escaped.txt": "gotcha",
	})
	store := NewStore("", filepath.Join(base, "cfg"))
	if _, err := store.Install(zipPath); err == nil {
		t.Fatal("越界的压缩包应当被拒绝")
	}
	if _, err := os.Stat(filepath.Join(base, "cfg", "escaped.txt")); err == nil {
		t.Fatal("越界的文件真的被写出来了")
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	for name, body := range files {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
