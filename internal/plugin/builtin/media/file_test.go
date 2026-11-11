package media

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/imbot"
	"wen/internal/llm"
	"wen/internal/plugin"
)

func fileTool(t *testing.T, p *Plugin) plugin.Tool {
	t.Helper()
	for _, tool := range p.Tools() {
		if tool.Name() == "send_file" {
			return tool
		}
	}
	t.Fatal("send_file 工具不存在")
	return nil
}

func executeFile(t *testing.T, p *Plugin, path string) (string, error) {
	t.Helper()
	return fileTool(t, p).Execute(context.Background(), []byte(`{"path":`+jsonString(path)+`}`))
}

type fileRecord struct {
	userID  string
	replyTo string
	file    llm.FilePart
}

type fileSender struct {
	mu    sync.Mutex
	files []fileRecord
	ch    chan fileRecord
	texts chan string
	err   error
}

func (f *fileSender) Send(_ context.Context, _, text, _ string) {
	select {
	case f.texts <- text:
	default:
	}
}

func (f *fileSender) SendFile(_ context.Context, userID, replyTo string, file llm.FilePart) error {
	if f.err != nil {
		return f.err
	}
	rec := fileRecord{userID: userID, replyTo: replyTo, file: file}
	f.mu.Lock()
	f.files = append(f.files, rec)
	f.mu.Unlock()
	select {
	case f.ch <- rec:
	default:
	}
	return nil
}

func TestSendFileRejectsOutsideWorkdir(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	path := writeFile(t, outside, "a.txt", []byte("hello"))
	p := newInited(t, work)

	_, err := executeFile(t, p, path)
	if err == nil || !strings.Contains(err.Error(), "工作目录之外") {
		t.Fatalf("目录外文件应被拒绝，得到 %v", err)
	}
}

func TestSendFileRejectsMissingAndOversize(t *testing.T) {
	work := t.TempDir()
	p := newInited(t, work)

	if _, err := executeFile(t, p, "missing.txt"); err == nil || !strings.Contains(err.Error(), "找不到文件") {
		t.Fatalf("缺文件应报找不到，得到 %v", err)
	}

	writeFile(t, work, "huge.bin", make([]byte, imbot.MaxFileBytes+1))
	if _, err := executeFile(t, p, "huge.bin"); err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超限文件应被拒绝，得到 %v", err)
	}
}

func TestSendFileWithRelativeWorkdir(t *testing.T) {
	abs := t.TempDir()
	writeFile(t, abs, "a.txt", []byte("hello"))
	rel, err := filepath.Rel(".", abs)
	if err != nil {
		t.Skipf("当前目录无法相对化: %v", err)
	}
	p := newInited(t, rel)

	_, err = executeFile(t, p, "a.txt")
	if err == nil || !strings.Contains(err.Error(), "不是从 IM 通道发起") {
		t.Fatalf("相对工作目录应能解析到文件，得到 %v", err)
	}
}

func TestSendFileThroughTurn(t *testing.T) {
	imbot.Declare("media_file_test", "Media File Test")
	work := t.TempDir()
	writeFile(t, work, "a.txt", []byte("hello"))
	p := newInited(t, work)
	tool := fileTool(t, p)

	sender := &fileSender{ch: make(chan fileRecord, 1)}
	core, err := imbot.New(imbot.Config{
		PluginName: "media_file_test",
		Sender:     sender,
		Lang:       "zh",
		StateDir:   t.TempDir(),
		Allow:      func(string) bool { return true },
		RunTurn: func(ctx context.Context, _, _ string) (string, error) {
			ctx = plugin.WithTurnOrigin(ctx, "media_file_test")
			_, err := tool.Execute(ctx, []byte(`{"path":"a.txt"}`))
			return "ok", err
		},
		NewSession: func() (string, error) { return "sess-1", nil },
		Sessions:   fakeSessions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	core.Start(context.Background())
	t.Cleanup(core.Stop)

	core.Handle(context.Background(), imbot.Message{UserID: "u1", Text: "发个文件", ReplyTo: "r1"})
	select {
	case got := <-sender.ch:
		if got.userID != "u1" || got.replyTo != "r1" || got.file.Name != "a.txt" {
			t.Fatalf("投递结果不符: %+v", got)
		}
		if string(got.file.Data) != "hello" {
			t.Fatal("投递的文件字节不一致")
		}
		if got.file.MediaType != "text/plain; charset=utf-8" {
			t.Fatalf("MediaType = %q", got.file.MediaType)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待发送文件超时")
	}
}

func TestSendFileDeliveryUnknownKeepsHint(t *testing.T) {
	imbot.Declare("media_file_unknown", "Media File Unknown")
	work := t.TempDir()
	writeFile(t, work, "a.txt", []byte("hello"))
	p := newInited(t, work)
	tool := fileTool(t, p)

	sender := &fileSender{
		ch:    make(chan fileRecord, 1),
		texts: make(chan string, 4),
		err:   imbot.ErrMediaDeliveryUnknown,
	}
	core, err := imbot.New(imbot.Config{
		PluginName: "media_file_unknown",
		Sender:     sender,
		Lang:       "zh",
		StateDir:   t.TempDir(),
		Allow:      func(string) bool { return true },
		RunTurn: func(ctx context.Context, _, _ string) (string, error) {
			ctx = plugin.WithTurnOrigin(ctx, "media_file_unknown")
			_, err := tool.Execute(ctx, []byte(`{"path":"a.txt"}`))
			return "", err
		},
		NewSession: func() (string, error) { return "sess-1", nil },
		Sessions:   fakeSessions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	core.Start(context.Background())
	t.Cleanup(core.Stop)

	core.Handle(context.Background(), imbot.Message{UserID: "u1", Text: "发个文件", ReplyTo: "r1"})
	select {
	case text := <-sender.texts:
		if !strings.Contains(text, "可能已送达") {
			t.Fatalf("应保留送达未知的提示，得到 %q", text)
		}
		if strings.Contains(text, "检查通道配置") {
			t.Fatalf("不应压成普通发送失败，得到 %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待错误回复超时")
	}
}
