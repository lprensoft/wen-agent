package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"wen/internal/llm"
	"wen/internal/plugin"
)

func TestRunWithImagesPersistsAndReloads(t *testing.T) {
	primary := &chainProvider{events: contentEvents("收到")}
	ag, id := fallbackAgent(t, primary, &chainProvider{}, &translatorPlugin{})
	data := []byte("fake-png-bytes")

	ag.RunWithImages(plugin.WithInteractive(context.Background()), id, "看图", []llm.ImagePart{
		{MediaType: "image/png", Data: data},
	}, func(Event) {})

	_, history, err := ag.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	for _, m := range history {
		if m.Role != llm.RoleUser || len(m.Images) == 0 {
			continue
		}
		if len(m.Images[0].Data) != 0 {
			t.Fatal("image bytes must not be stored in the JSONL message")
		}
		if m.Images[0].Name == "" || m.Images[0].MediaType != "image/png" {
			t.Fatalf("image ref = %+v", m.Images[0])
		}
		name = m.Images[0].Name
	}
	if name == "" {
		t.Fatal("user message did not keep an image reference")
	}
	if got, err := ag.store.ReadImage(id, name); err != nil || string(got) != string(data) {
		t.Fatalf("stored image = %q, %v", got, err)
	}

	// 第二轮必须从会话附件目录把字节读回来再发给模型。
	ag.Run(plugin.WithInteractive(context.Background()), id, "继续", func(Event) {})
	if len(primary.reqs) < 2 {
		t.Fatalf("requests = %d", len(primary.reqs))
	}
	found := false
	for _, m := range primary.reqs[1].Messages {
		if len(m.Images) == 0 {
			continue
		}
		found = true
		if string(m.Images[0].Data) != string(data) {
			t.Fatalf("hydrated image = %q", m.Images[0].Data)
		}
	}
	if !found {
		t.Fatal("second request lost the image")
	}
}

func TestRunWithFilesPersistsPathNote(t *testing.T) {
	primary := &chainProvider{events: contentEvents("收到")}
	ag, id := fallbackAgent(t, primary, &chainProvider{}, &translatorPlugin{})

	if _, err := ag.RunTurnWithMedia(plugin.WithInteractive(context.Background()), id, "", nil, []llm.FilePart{
		{Name: "报告.txt", MediaType: "text/plain", Data: []byte("hello")},
	}); err != nil {
		t.Fatalf("RunTurnWithMedia: %v", err)
	}

	_, history, err := ag.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var user llm.Message
	for _, m := range history {
		if m.Role == llm.RoleUser && len(m.Files) > 0 {
			user = m.Message
		}
	}
	if len(user.Files) != 1 || user.Files[0].Name != "报告.txt" || user.Files[0].Stored == "" {
		t.Fatalf("user message did not keep a file reference: %+v", user.Files)
	}
	if strings.Contains(user.Content, "已保存到 ") {
		t.Fatalf("absolute path must not be persisted in the message: %q", user.Content)
	}
	path, err := ag.store.FilePath(id, user.Files[0].Stored)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "hello" {
		t.Fatalf("stored file = %q, %v", got, err)
	}

	// 本轮请求必须带上按当前会话目录解析出的路径；后续轮次从历史里的引用重新生成。
	if len(primary.reqs) != 1 {
		t.Fatalf("requests = %d", len(primary.reqs))
	}
	assertFileNote := func(req llm.ChatRequest) {
		t.Helper()
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "报告.txt") && strings.Contains(m.Content, path) {
				return
			}
		}
		t.Fatalf("request did not carry the resolved file path %s", path)
	}
	assertFileNote(primary.reqs[0])

	ag.Run(plugin.WithInteractive(context.Background()), id, "继续", func(Event) {})
	if len(primary.reqs) < 2 {
		t.Fatalf("requests = %d", len(primary.reqs))
	}
	assertFileNote(primary.reqs[1])
}
