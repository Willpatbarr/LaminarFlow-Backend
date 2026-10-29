package attachment

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveType(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	zip := []byte("PK\x03\x04\x14\x00\x00\x00")
	docx := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

	for _, tc := range []struct {
		name     string
		declared string
		head     string
		want     string
		refused  bool
	}{
		{"png", "image/png", string(png), "image/png", false},
		{"png declared as pdf is still png", "application/pdf", string(png), "image/png", false},
		{"png declared as nothing", "", string(png), "image/png", false},
		{"plain text", "", "hello", "text/plain", false},
		{"markdown by declaration", "text/markdown", "# hi", "text/markdown", false},
		{"markdown params dropped", "text/markdown; charset=utf-8", "# hi", "text/markdown", false},
		{"text declared as an image stays text", "image/png", "just words", "text/plain", false},
		{"docx by declaration", docx, string(zip), docx, false},
		{"zip with no declaration", "", string(zip), "application/zip", false},
		{"zip declared as docx-lookalike pdf", "application/pdf", string(zip), "application/zip", false},
		{"unknown binary", "", "\x00\x01\x02\x03\xff\xfe", "application/octet-stream", false},
		{"pdf", "", "%PDF-1.7\n", "application/pdf", false},

		{"html", "", "<!DOCTYPE html><html>", "", true},
		{"html upper case tag", "", "  <HTML><body>", "", true},
		{"script tag", "", "<script>alert(1)</script>", "", true},
		{"xml (svg with a prolog)", "", `<?xml version="1.0"?><svg/>`, "", true},
		{"svg declared", "image/svg+xml", "<svg/>", "", true},
		{"html declared", "text/html", "hi", "", true},
		{"javascript declared", "application/javascript", "x", "", true},
		{"wav audio", "", "RIFF\x24\x00\x00\x00WAVEfmt ", "", true},
	} {
		got, err := resolveType(tc.declared, []byte(tc.head))
		switch {
		case tc.refused && !errors.Is(err, ErrTypeNotAllowed):
			t.Errorf("%s: = %q, %v; want ErrTypeNotAllowed", tc.name, got, err)
		case !tc.refused && (err != nil || got != tc.want):
			t.Errorf("%s: = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestOnlyRasterImagesAreInline(t *testing.T) {
	for ct, want := range map[string]bool{
		"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
		"application/pdf": false, "text/plain": false, "application/octet-stream": false,
		"image/svg+xml": false, "text/html": false,
	} {
		if Inline(ct) != want {
			t.Errorf("Inline(%q) = %v, want %v", ct, !want, want)
		}
	}
	if Allowed("image/svg+xml") || Allowed("text/html") {
		t.Error("svg or html is allowed")
	}
}

func TestSanitizeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":                   "report.pdf",
		"../../etc/passwd":             "passwd",
		`C:\Users\me\secret.txt`:       "secret.txt",
		"a/b/c.png":                    "c.png",
		"..":                           "file",
		"":                             "file",
		"   ":                          "file",
		"line\r\nbreak.txt":            "linebreak.txt",
		"nul\x00byte.png":              "nulbyte.png",
		"  .hidden.  ":                 "hidden",
		"rtl\u202egnp.exe":             "rtlgnp.exe",
		"résumé 日本.txt":                "résumé 日本.txt",
		"bad\xffutf8.txt":              "badutf8.txt",
		strings.Repeat("a", 400) + "x": strings.Repeat("a", 255),
	} {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}

	long := SanitizeFilename(strings.Repeat("é", 300))
	if len(long) > MaxFilenameBytes || strings.ContainsRune(long, '\uFFFD') {
		t.Errorf("a long multibyte name was cut badly: %d bytes", len(long))
	}
}
