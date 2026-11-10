package server

import (
	"encoding/base64"
	"testing"
)

func TestNormalizeChatImages(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEElEQVR4nGL6z8AACAAA//8DCQECWLbVUAAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	out, msg := normalizeChatImages("", []chatImage{{MediaType: "text/plain", Data: png}})
	if msg != "" || len(out) != 1 {
		t.Fatalf("valid PNG rejected: %q %+v", msg, out)
	}
	if out[0].MediaType != "image/png" {
		t.Fatalf("media type = %q, want detected image/png", out[0].MediaType)
	}

	if _, msg := normalizeChatImages("", []chatImage{{Data: []byte("not an image")}}); msg == "" {
		t.Fatal("non-image data should be rejected")
	}
	corrupt := append([]byte("\x89PNG\r\n\x1a\n"), []byte("fake")...)
	if _, msg := normalizeChatImages("", []chatImage{{Data: corrupt}}); msg == "" {
		t.Fatal("a valid signature with corrupt body should be rejected")
	}
	if _, msg := normalizeChatImages("", []chatImage{{Data: nil}}); msg == "" {
		t.Fatal("empty image should be rejected")
	}
}
