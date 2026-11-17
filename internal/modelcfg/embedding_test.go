package modelcfg

import (
	"os"
	"strings"
	"testing"

	"wen/internal/llm"
)

func cloudEmbedding() Embedding {
	return Embedding{Enabled: true, Type: llm.EmbedTypeOpenAI, BaseURL: "https://api.example.com/v1/",
		APIKey: "sk-embed", Model: "text-embedding-3-small"}
}

func TestEmbeddingDefaultsToOff(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	if s.Embedding().Active() {
		t.Fatal("没配过嵌入就必须是关的：启用会把记忆正文发给第三方")
	}
}

func TestEmbeddingRoundTripAndReload(t *testing.T) {
	s, path := newStore(t, baseConfig())
	saved, err := s.SetEmbedding("", cloudEmbedding())
	if err != nil {
		t.Fatal(err)
	}
	if saved.BaseURL != "https://api.example.com/v1" {
		t.Errorf("末尾斜杠应被去掉：%q", saved.BaseURL)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o077 != 0 && info.Mode().Perm() != 0o666 { // Windows 不区分权限位
		t.Errorf("含明文密钥的文件权限 = %v", info.Mode().Perm())
	}
	s2, err := NewStore("", path, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Embedding(); got != saved || !got.Active() {
		t.Fatalf("重载后 = %+v", got)
	}
}

func TestSaveModelsDoesNotWipeEmbedding(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	if _, err := s.SetEmbedding("", cloudEmbedding()); err != nil {
		t.Fatal(err)
	}
	// 设置页保存提供商时提交的整档里没有 embedding，甚至可能带着伪造的
	doc := s.Snapshot()
	doc.Embedding = &Embedding{Enabled: true, BaseURL: "https://evil.example.com", Type: llm.EmbedTypeOpenAI, Model: "x", APIKey: "k"}
	if _, err := s.Save("", doc); err != nil {
		t.Fatal(err)
	}
	got := s.Embedding()
	if got.BaseURL != "https://api.example.com/v1" || got.APIKey != "sk-embed" {
		t.Fatalf("保存提供商不该动嵌入配置：%+v", got)
	}
}

func TestEmbeddingEmptyKeyKeptOnlyForSameBaseURL(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	if _, err := s.SetEmbedding("", cloudEmbedding()); err != nil {
		t.Fatal(err)
	}
	same := cloudEmbedding()
	same.APIKey = ""
	same.Model = "text-embedding-3-large"
	got, err := s.SetEmbedding("", same)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "sk-embed" {
		t.Errorf("地址没变，留空应沿用旧密钥：%q", got.APIKey)
	}

	moved := cloudEmbedding()
	moved.APIKey = ""
	moved.BaseURL = "https://other.example.com/v1"
	if _, err := s.SetEmbedding("", moved); err == nil {
		t.Fatal("地址换了还沿用旧密钥，等于把密钥发给另一个主人")
	}
}

func TestValidateEmbedding(t *testing.T) {
	ok := cloudEmbedding()
	bad := map[string]func(*Embedding){
		"未知接口":    func(e *Embedding) { e.Type = "nope" },
		"地址格式错":   func(e *Embedding) { e.BaseURL = "api.example.com" },
		"没填模型":    func(e *Embedding) { e.Model = "" },
		"远端没密钥":   func(e *Embedding) { e.APIKey = "" },
		"维度为负":    func(e *Embedding) { e.Dimensions = -1 },
		"维度过大":    func(e *Embedding) { e.Dimensions = maxEmbedDimensions + 1 },
		"开启但没选接口": func(e *Embedding) { e.Type = "" },
	}
	for name, mutate := range bad {
		e := ok
		mutate(&e)
		if err := ValidateEmbedding("", e); err == nil {
			t.Errorf("%s：应当报错", name)
		}
	}

	local := Embedding{Enabled: true, Type: llm.EmbedTypeOllama, BaseURL: "http://localhost:11434", Model: "bge-m3"}
	if err := ValidateEmbedding("", local); err != nil {
		t.Errorf("本机服务不要求密钥：%v", err)
	}
	// 关着的配置可以写到一半
	if err := ValidateEmbedding("", Embedding{Type: llm.EmbedTypeOpenAI}); err != nil {
		t.Errorf("关着的半成品配置不该被挡住：%v", err)
	}
}

func TestDisabledEmbeddingIsNotActive(t *testing.T) {
	e := cloudEmbedding()
	e.Enabled = false
	if e.Active() {
		t.Fatal("填好但没开启 = 没配")
	}
}

func TestEmptyEmbeddingLeavesNoShellInFile(t *testing.T) {
	s, path := newStore(t, baseConfig())
	if _, err := s.SetEmbedding("", cloudEmbedding()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetEmbedding("", Embedding{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), `"embedding"`) {
		t.Fatalf("全空配置不该在文件里留一段空壳：%s", raw)
	}
}
