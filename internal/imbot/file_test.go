package imbot

import (
	"strings"
	"testing"

	"wen/internal/llm"
)

func TestFileFromBytes(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n")
	tests := []struct {
		name      string
		data      []byte
		rawName   string
		mediaType string
		wantName  string
		wantType  string
		wantErr   bool
	}{
		{name: "按扩展名推断", data: []byte("hello"), rawName: "a.txt", wantName: "a.txt", wantType: "text/plain; charset=utf-8"},
		{name: "通道给的 MIME 优先", data: []byte("hello"), rawName: "a.bin", mediaType: "text/csv", wantName: "a.bin", wantType: "text/csv"},
		{name: "图片扩展名", data: png, rawName: "a.png", wantName: "a.png", wantType: "image/png"},
		{name: "去掉路径", data: []byte("x"), rawName: "../../etc/passwd", wantName: "passwd", wantType: "text/plain; charset=utf-8"},
		{name: "替换不安全字符", data: []byte("x"), rawName: `a?b.txt`, wantName: "a_b.txt", wantType: "text/plain; charset=utf-8"},
		{name: "空内容", data: nil, rawName: "a.txt", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FileFromBytes(tt.data, tt.rawName, tt.mediaType)
			if tt.wantErr {
				if err == nil {
					t.Fatal("应当报错")
				}
				return
			}
			if err != nil {
				t.Fatalf("FileFromBytes: %v", err)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if got.MediaType != tt.wantType {
				t.Errorf("MediaType = %q, want %q", got.MediaType, tt.wantType)
			}
			if got.Size != len(tt.data) {
				t.Errorf("Size = %d, want %d", got.Size, len(tt.data))
			}
		})
	}
}

func TestFileFromBytesRejectsOversize(t *testing.T) {
	if _, err := FileFromBytes(make([]byte, MaxFileBytes+1), "a.bin", ""); err == nil {
		t.Fatal("超过上限的文件应报错")
	}
}

func TestFileNameTruncatesLongName(t *testing.T) {
	f := (llm.FilePart{Name: strings.Repeat("名", 200) + ".txt"}).FileName()
	if len([]rune(f)) > 120 {
		t.Fatalf("文件名过长: %d 个字符", len([]rune(f)))
	}
	if !strings.HasSuffix(f, ".txt") {
		t.Fatalf("应保留扩展名，得到 %q", f)
	}
}
