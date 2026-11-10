package llm

import "testing"

func TestImageFileNameUsesActualMediaType(t *testing.T) {
	cases := []struct {
		name      string
		mediaType string
		want      string
	}{
		{"a.png", "image/png", "a.png"},
		{"renamed.jpg", "image/png", "renamed.png"},
		{"a", "image/png", "a.png"},
		{"a.gif", "image/jpeg", "a.jpg"},
		{"a.JPEG", "image/jpeg", "a.JPEG"},
		{"a.PNG", "image/png", "a.PNG"},
		{"", "image/png", "image.png"},
		{"dir/a.png", "image/png", "a.png"},
		{".png", "image/png", "image.png"},
		{"a.png", "", "a.png"},
	}
	for _, c := range cases {
		got := (ImagePart{Name: c.name, MediaType: c.mediaType}).FileName()
		if got != c.want {
			t.Errorf("FileName(Name=%q, MediaType=%q) = %q, want %q", c.name, c.mediaType, got, c.want)
		}
	}
}
