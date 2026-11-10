package media

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/imbot"
	"wen/internal/llm"
	"wen/internal/plugin"
)

var testPNG = func() []byte {
	b, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEElEQVR4nGL6z8AACAAA//8DCQECWLbVUAAAAABJRU5ErkJggg==")
	return b
}()

func newInited(t *testing.T, workdir string) *Plugin {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{Workdir: workdir, Lang: "zh"}, nil); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func execute(t *testing.T, p *Plugin, path string) (string, error) {
	t.Helper()
	args := []byte(`{"path":` + jsonString(path) + `}`)
	return p.Tools()[0].Execute(context.Background(), args)
}

func jsonString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func TestSendImageRejectsOutsideWorkdir(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	path := writeFile(t, outside, "a.png", testPNG)
	p := newInited(t, work)

	_, err := execute(t, p, path)
	if err == nil || !strings.Contains(err.Error(), "工作目录之外") {
		t.Fatalf("目录外图片应被拒绝，得到 %v", err)
	}
}

func TestSendImageRejectsSymlinkEscape(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	target := writeFile(t, outside, "secret.png", testPNG)
	link := filepath.Join(work, "link.png")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("当前环境不能创建符号链接: %v", err)
	}
	p := newInited(t, work)

	_, err := execute(t, p, "link.png")
	if err == nil || !strings.Contains(err.Error(), "工作目录之外") {
		t.Fatalf("符号链接逃逸应被拒绝，得到 %v", err)
	}
}

func TestSendImageRejectsMissingAndCorrupt(t *testing.T) {
	work := t.TempDir()
	p := newInited(t, work)

	if _, err := execute(t, p, "missing.png"); err == nil || !strings.Contains(err.Error(), "找不到图片") {
		t.Fatalf("缺文件应报找不到，得到 %v", err)
	}

	writeFile(t, work, "bad.png", []byte("not an image"))
	if _, err := execute(t, p, "bad.png"); err == nil || !strings.Contains(err.Error(), "不是可用的") {
		t.Fatalf("损坏图片应被拒绝，得到 %v", err)
	}
}

func TestSendImageRejectsOversize(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "huge.png", make([]byte, imbot.MaxImageBytes+1))
	p := newInited(t, work)

	_, err := execute(t, p, "huge.png")
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超限图片应被拒绝，得到 %v", err)
	}
}

func TestSendImageWithoutIMTurn(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, "a.png", testPNG)
	p := newInited(t, work)

	_, err := execute(t, p, "a.png")
	if err == nil || !strings.Contains(err.Error(), "不是从 IM 通道发起") {
		t.Fatalf("Web UI 轮次应明确拒绝，得到 %v", err)
	}
}

// ---------- 与骨架的联通 ----------

type imageRecord struct {
	userID  string
	replyTo string
	img     llm.ImagePart
}

type imageSender struct {
	mu     sync.Mutex
	images []imageRecord
	ch     chan imageRecord
	texts  chan string
	err    error
}

func (f *imageSender) Send(_ context.Context, _, text, _ string) {
	select {
	case f.texts <- text:
	default:
	}
}

func (f *imageSender) SendImage(_ context.Context, userID, replyTo string, img llm.ImagePart) error {
	if f.err != nil {
		return f.err
	}
	rec := imageRecord{userID: userID, replyTo: replyTo, img: img}
	f.mu.Lock()
	f.images = append(f.images, rec)
	f.mu.Unlock()
	select {
	case f.ch <- rec:
	default:
	}
	return nil
}

type fakeSessions struct{}

func (fakeSessions) LastActive() (string, time.Time, error) { return "", time.Time{}, nil }
func (fakeSessions) LastInteraction() (time.Time, error)    { return time.Time{}, nil }
func (fakeSessions) Exists(string) bool                    { return true }

// TestSendImageThroughTurn 走完整链路：Core 把入站消息放进轮次 ctx → 工具读图
// → SendTurnImage 用本轮用户与回复凭据交给通道。
func TestSendImageThroughTurn(t *testing.T) {
	imbot.Declare("media_test", "Media Test")
	work := t.TempDir()
	writeFile(t, work, "a.png", testPNG)
	p := newInited(t, work)
	tool := p.Tools()[0]

	sender := &imageSender{ch: make(chan imageRecord, 1)}
	core, err := imbot.New(imbot.Config{
		PluginName: "media_test",
		Sender:     sender,
		Lang:       "zh",
		StateDir:   t.TempDir(),
		Allow:      func(string) bool { return true },
		RunTurn: func(ctx context.Context, _, _ string) (string, error) {
			ctx = plugin.WithTurnOrigin(ctx, "media_test")
			_, err := tool.Execute(ctx, []byte(`{"path":"a.png"}`))
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

	core.Handle(context.Background(), imbot.Message{UserID: "u1", Text: "发张图", ReplyTo: "r1"})
	select {
	case got := <-sender.ch:
		if got.userID != "u1" || got.replyTo != "r1" || got.img.MediaType != "image/png" {
			t.Fatalf("投递结果不符: %+v", got)
		}
		if string(got.img.Data) != string(testPNG) {
			t.Fatal("投递的图片字节不一致")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待发图超时")
	}
}

// TestSendImageDeliveryUnknownKeepsHint 保证通道报「送达状态未知」时，工具层
// 不会把它压成普通的「发送失败」，否则模型可能再发一次造成重复。
func TestSendImageDeliveryUnknownKeepsHint(t *testing.T) {
	imbot.Declare("media_unknown", "Media Unknown")
	work := t.TempDir()
	writeFile(t, work, "a.png", testPNG)
	p := newInited(t, work)
	tool := p.Tools()[0]

	sender := &imageSender{
		ch:    make(chan imageRecord, 1),
		texts: make(chan string, 4),
		err:   imbot.ErrImageDeliveryUnknown,
	}
	core, err := imbot.New(imbot.Config{
		PluginName: "media_unknown",
		Sender:     sender,
		Lang:       "zh",
		StateDir:   t.TempDir(),
		Allow:      func(string) bool { return true },
		RunTurn: func(ctx context.Context, _, _ string) (string, error) {
			ctx = plugin.WithTurnOrigin(ctx, "media_unknown")
			_, err := tool.Execute(ctx, []byte(`{"path":"a.png"}`))
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

	core.Handle(context.Background(), imbot.Message{UserID: "u1", Text: "发张图", ReplyTo: "r1"})
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
