package plugin

import (
	"strings"
	"testing"
)

func TestStripHTML(t *testing.T) {
	in := `<!doctype html><html><head><title>标题</title>
<script>var x = "不应出现";</script><style>body{color:red}</style></head>
<body><h1>你好</h1><p>第一段内容</p><p>第二段 &amp; 符号</p></body></html>`

	out := StripHTML(in)
	for _, want := range []string{"你好", "第一段内容", "第二段 & 符号"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, ban := range []string{"<script", "不应出现", "color:red", "<p>", "&amp;"} {
		if strings.Contains(out, ban) {
			t.Errorf("should not contain %q in:\n%s", ban, out)
		}
	}
}
