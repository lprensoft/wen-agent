package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore("", filepath.Join(t.TempDir(), "memories"))
}

func TestSaveAndGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	saved, err := s.Save(Entry{
		Name:        "提交信息用中文",
		Description: "提交信息写中文，说明做了什么和为什么",
		Type:        "约定",
		Content:     "正文第一行\n\n正文第二段",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Slug != "提交信息用中文" {
		t.Errorf("slug = %q", saved.Slug)
	}

	got, err := s.Get("提交信息用中文")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "提交信息写中文，说明做了什么和为什么" || got.Type != "约定" {
		t.Errorf("frontmatter 未往返: %+v", got)
	}
	if got.Content != "正文第一行\n\n正文第二段" {
		t.Errorf("正文未往返: %q", got.Content)
	}
	if got.Created.IsZero() || got.Updated.IsZero() {
		t.Errorf("时间戳缺失: %+v", got)
	}

	// 按文件名查找同样应命中，且大小写不敏感
	if _, err := s.Get("提交信息用中文"); err != nil {
		t.Error(err)
	}
	if _, err := s.Get("不存在的记忆"); err == nil {
		t.Error("查找不存在的记忆应报错")
	}
}

func TestGetAcceptsIndexDisplayForm(t *testing.T) {
	s := newTestStore(t)
	s.Save(Entry{Name: "接口命名规范", Description: "钩子", Type: "约定", Content: "正文"}, false)

	// 索引里显示为「分类/标题」，模型很自然会把整串当标题传回来
	for _, name := range []string{"接口命名规范", "约定/接口命名规范", " 约定 / 接口命名规范 "} {
		if _, err := s.Get(name); err != nil {
			t.Errorf("Get(%q) 应命中: %v", name, err)
		}
	}
	// 不是已知分类的前缀不应被吞掉，否则会误伤标题里本来就有斜杠的记忆
	if _, err := s.Get("随便/接口命名规范"); err == nil {
		t.Error("非分类前缀不应被剥离")
	}
}

func TestSaveRejectsOverwriteWithoutReplace(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Save(Entry{Name: "配置", Type: "事实", Content: "v1"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(Entry{Name: "配置", Type: "事实", Content: "v2"}, false); err == nil {
		t.Fatal("默认不应覆盖同名记忆")
	}
	if got, _ := s.Get("配置"); got.Content != "v1" {
		t.Errorf("被拒绝的保存不应改动内容: %q", got.Content)
	}

	first, _ := s.Get("配置")
	if _, err := s.Save(Entry{Name: "配置", Type: "事实", Content: "v2"}, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("配置")
	if got.Content != "v2" {
		t.Errorf("覆盖后内容 = %q", got.Content)
	}
	if !got.Created.Equal(first.Created) {
		t.Error("覆盖应沿用原创建时间")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "配置.md.bak")); err != nil {
		t.Error("覆盖前应留下 .bak 备份")
	}
	// .bak 不是 .md，不应被当成一条记忆
	if all, _ := s.List(); len(all) != 1 {
		t.Errorf("备份文件不应进入列表: %d 条", len(all))
	}
}

func TestSaveValidatesTypeAndTruncatesDescription(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Save(Entry{Name: "x", Type: "胡说", Content: "c"}, false); err == nil {
		t.Error("未知分类应被拒绝")
	}
	if _, err := s.Save(Entry{Name: "   ", Type: "事实"}, false); err == nil {
		t.Error("空标题应被拒绝")
	}

	long := strings.Repeat("很长的描述", 20)
	saved, err := s.Save(Entry{Name: "y", Description: long, Type: "事实", Content: "c"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(saved.Description)); n != descMaxRunes {
		t.Errorf("描述应被截断到 %d 个字符，实际 %d", descMaxRunes, n)
	}

	// 多行描述会破坏索引的一行一条，必须被压平
	saved, _ = s.Save(Entry{Name: "z", Description: "第一行\n第二行\t带  空白", Type: "事实", Content: "c"}, false)
	if strings.ContainsAny(saved.Description, "\n\t") || strings.Contains(saved.Description, "  ") {
		t.Errorf("描述应被压成单行: %q", saved.Description)
	}
}

func TestListSortedByTypeThenName(t *testing.T) {
	s := newTestStore(t)
	for _, e := range []Entry{
		{Name: "b约定", Type: "约定"},
		{Name: "a事实", Type: "事实"},
		{Name: "a约定", Type: "约定"},
		{Name: "偏好项", Type: "偏好"},
	} {
		e.Content = "c"
		if _, err := s.Save(e, false); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"偏好项", "a约定", "b约定", "a事实"} // 偏好 < 约定 < 事实
	if len(got) != len(want) {
		t.Fatalf("got %d entries", len(got))
	}
	for i, n := range want {
		if got[i].Name != n {
			t.Errorf("第 %d 条 = %q, want %q", i, got[i].Name, n)
		}
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	s.Save(Entry{Name: "临时", Type: "事实", Content: "c"}, false)
	if _, err := s.Delete("临时"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.List(); len(all) != 0 {
		t.Errorf("删除后仍有 %d 条", len(all))
	}
	if _, err := s.Delete("临时"); err == nil {
		t.Error("删除不存在的记忆应报错")
	}
}

func TestListOnMissingDirIsEmpty(t *testing.T) {
	s := NewStore("", filepath.Join(t.TempDir(), "不存在"))
	got, err := s.List()
	if err != nil {
		t.Fatalf("目录不存在不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries", len(got))
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"提交信息用中文", "提交信息用中文"},
		{"read file 用法", "read-file-用法"},
		{"a/b\\c:d*e?f", "a-b-c-d-e-f"},
		{"  前后空格  ", "前后空格"},
		{"结尾的点...", "结尾的点"},
		{"结尾空格 ", "结尾空格"},
		{"多个///分隔", "多个-分隔"},
		{"keep_under-score", "keep_under-score"},
		{"CON", "_CON"}, // 落盘会变成 CON.md，Windows 仍视为设备名
		{"nul", "_nul"},
		{"con.md", "con-md"}, // 点被折成连字符后不再是保留名
		{"comm", "comm"},     // 不是保留名，不加前缀
	}
	for _, c := range cases {
		got, err := slugify("", c.in)
		if err != nil {
			t.Errorf("slugify(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	if _, err := slugify("", "///"); err == nil {
		t.Error("不含可用字符的标题应报错")
	}
	if _, err := slugify("", ""); err == nil {
		t.Error("空标题应报错")
	}

	long, err := slugify("", strings.Repeat("长", 200))
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(long)); n != slugMaxRunes {
		t.Errorf("超长标题应被截到 %d 个字符，实际 %d", slugMaxRunes, n)
	}
}

func TestSlugCollisionCaseInsensitive(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Save(Entry{Name: "Config", Type: "事实", Content: "v1"}, false); err != nil {
		t.Fatal(err)
	}
	// NTFS 大小写不敏感、Linux 敏感；统一按不敏感判重，两个平台行为一致
	if _, err := s.Save(Entry{Name: "config", Type: "事实", Content: "v2"}, false); err == nil {
		t.Fatal("大小写不同的同名记忆应被判为已存在")
	}
	saved, err := s.Save(Entry{Name: "config", Type: "事实", Content: "v2"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Slug != "Config" {
		t.Errorf("覆盖应沿用原文件名，得到 %q", saved.Slug)
	}
	if all, _ := s.List(); len(all) != 1 {
		t.Errorf("不应产生第二个文件: %d 条", len(all))
	}
}

func TestReadToleratesHandEditedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memories")
	os.MkdirAll(dir, 0o755)
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// CRLF 行尾 + UTF-8 BOM：手工用记事本编辑后的典型形态
	write("crlf.md", "\xEF\xBB\xBF---\r\nname: CRLF 条目\r\ndescription: 带 BOM 与 CRLF\r\ntype: 事实\r\n---\r\n\r\n正文\r\n")
	// 正文里含 --- 分隔线，不能被当成 frontmatter 围栏
	write("rule.md", "---\nname: 含分隔线\ndescription: 钩子\ntype: 约定\n---\n\n上半部分\n\n---\n\n下半部分\n")
	// 完全没有 frontmatter 的普通笔记
	write("plain.md", "这是第一行\n这是第二行\n")
	// frontmatter 语法损坏，应退回纯文本而不是丢掉整条
	write("broken.md", "---\nname: [未闭合\n---\n\n正文\n")

	s := NewStore("", dir)
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("应读到 4 条，实际 %d", len(all))
	}

	byName := map[string]Entry{}
	for _, e := range all {
		byName[e.Name] = e
	}
	if e := byName["CRLF 条目"]; e.Description != "带 BOM 与 CRLF" || e.Content != "正文" {
		t.Errorf("CRLF/BOM 解析错误: %+v", e)
	}
	if e := byName["含分隔线"]; !strings.Contains(e.Content, "上半部分") || !strings.Contains(e.Content, "下半部分") {
		t.Errorf("正文里的分隔线被误判为围栏: %q", e.Content)
	}
	if e := byName["plain"]; e.Description != "这是第一行" {
		t.Errorf("无 frontmatter 的文件应用首行做钩子: %+v", e)
	}
	if _, ok := byName["broken"]; !ok {
		t.Errorf("frontmatter 损坏的文件应退回纯文本，实际条目: %v", byName)
	}
}

func TestCacheInvalidatedByExternalEdit(t *testing.T) {
	s := newTestStore(t)
	s.Save(Entry{Name: "外部编辑", Type: "事实", Content: "旧"}, false)
	if got, _ := s.Get("外部编辑"); got.Content != "旧" {
		t.Fatalf("content = %q", got.Content)
	}

	// 模拟用户在进程外改文件：目录指纹变化应让缓存失效
	p := filepath.Join(s.Dir(), "外部编辑.md")
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(raw), "旧", "新内容", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, time.Now().Add(time.Second), time.Now().Add(time.Second))

	if got, _ := s.Get("外部编辑"); got.Content != "新内容" {
		t.Errorf("外部修改后仍读到旧内容: %q", got.Content)
	}
}

func TestConcurrentSaveSameSlug(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Save(Entry{Name: "热点", Type: "事实", Content: "初始"}, false); err != nil {
		t.Fatal(err)
	}

	const n = 40
	var wg sync.WaitGroup
	for i := range n {
		// 并发覆盖同一条 + 并发读，不应写坏文件或读到半截内容
		wg.Go(func() {
			if _, err := s.Save(Entry{
				Name: "热点", Type: "事实", Content: fmt.Sprintf("内容-%02d", i),
			}, true); err != nil {
				t.Errorf("save: %v", err)
			}
			if _, err := s.List(); err != nil {
				t.Errorf("list: %v", err)
			}
		})
	}
	wg.Wait()

	got, err := s.Get("热点")
	if err != nil {
		t.Fatal(err)
	}
	// "内容-NN" 共 5 个字符，长度不对就说明读到了半截或拼接的内容
	if !strings.HasPrefix(got.Content, "内容-") || len([]rune(got.Content)) != 5 {
		t.Errorf("并发写后内容不完整: %q", got.Content)
	}
	if all, _ := s.List(); len(all) != 1 {
		t.Errorf("并发写不应产生额外文件: %d 条", len(all))
	}
	// 临时文件不应残留
	des, _ := os.ReadDir(s.Dir())
	for _, de := range des {
		if strings.HasSuffix(de.Name(), ".tmp") {
			t.Errorf("残留临时文件: %s", de.Name())
		}
	}
}
