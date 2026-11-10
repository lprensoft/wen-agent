package imbot

import (
	"encoding/base64"
	"testing"
)

func TestImageFromBytes(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEElEQVR4nGL6z8AACAAA//8DCQECWLbVUAAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	img, err := ImageFromBytes(png)
	if err != nil || img.MediaType != "image/png" || len(img.Data) != len(png) {
		t.Fatalf("img=%+v err=%v", img, err)
	}
	corrupt := append([]byte("\x89PNG\r\n\x1a\n"), []byte("fake")...)
	if _, err := ImageFromBytes(corrupt); err == nil {
		t.Fatal("signature-only image should be rejected")
	}
	if _, err := ImageFromBytes(nil); err == nil {
		t.Fatal("empty image should be rejected")
	}
}
