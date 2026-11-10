package agent

import (
	"context"
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
