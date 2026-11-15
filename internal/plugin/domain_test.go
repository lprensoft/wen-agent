package plugin

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDomainDir(t *testing.T) {
	base := filepath.Join("state", "memories")
	// 空标签用基准目录本身：升级前的数据不需要迁移，「共享」天然落在原位置
	if got := DomainDir(base, ""); got != base {
		t.Errorf("DomainDir(base, \"\") = %q, want %q", got, base)
	}
	if got, want := DomainDir(base, "inner"), base+"-inner"; got != want {
		t.Errorf("DomainDir = %q, want %q", got, want)
	}
	if got := DomainDir("", "inner"); got != "" {
		t.Errorf("没有基准目录时应返回空: %q", got)
	}
}

// 标签会被拼进磁盘路径，凡是能改变目录层级的字符都必须判为非法。
func TestValidDomainTag(t *testing.T) {
	for _, tag := range []string{"", "inner", "a1_b"} {
		if !ValidDomainTag(tag) {
			t.Errorf("%q 应当合法", tag)
		}
	}
	for _, tag := range []string{"x/../../../../outside", `x\..\outside`, "..", "Inner", "a-b", "-a", "域"} {
		if ValidDomainTag(tag) {
			t.Errorf("%q 应当非法", tag)
		}
	}
}

func TestReadDomainsPutsWriteFirst(t *testing.T) {
	// 写入域排在最前：同名数据在多个域都存在时以正在写入的那个为准
	got := ReadDomains("", Scope{Write: "inner", Read: []string{"outer", "inner"}})
	if !slices.Equal(got, []string{"inner", "", "outer"}) {
		t.Errorf("ReadDomains = %v", got)
	}
	// 空标签始终在列，且不重复
	got = ReadDomains("", Scope{Write: "outer", Read: []string{"outer"}})
	if !slices.Equal(got, []string{"outer", ""}) {
		t.Errorf("ReadDomains = %v", got)
	}
	// 零值：只有共享域
	got = ReadDomains("", Scope{Read: []string{}})
	if !slices.Equal(got, []string{""}) {
		t.Errorf("ReadDomains = %v", got)
	}
}

func TestReadDomainsEnumeratesWhenUnrestricted(t *testing.T) {
	base := filepath.Join(t.TempDir(), "archives")
	for _, dir := range []string{base, base + "-inner", base + "-outer", base + "-Bad", base + "-a-b", "unrelated"} {
		if err := os.MkdirAll(filepath.Join(filepath.Dir(base), filepath.Base(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 顺手放一个同前缀的文件，它不是目录，不该被当成可见域
	if err := os.WriteFile(base+"-notadir", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Read 为 nil 表示不限制，此时枚举已存在的域，与 CanRead 的语义保持一致
	got := ReadDomains(base, Scope{})
	if !slices.Equal(got, []string{"", "inner", "outer"}) {
		t.Errorf("ReadDomains = %v，应枚举出合规的域且按名排序", got)
	}
}

func TestReadDomainsMissingBaseDir(t *testing.T) {
	// 枚举失败应退化成「只有共享域」，不该报错阻断调用方
	got := ReadDomains(filepath.Join(t.TempDir(), "nope", "archives"), Scope{})
	if !slices.Equal(got, []string{""}) {
		t.Errorf("ReadDomains = %v", got)
	}
}
