package attachment

import (
	"mime"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// allowed is every content type an upload may be stored as, and whether a browser may
// be told to render it in place.
//
// Closed on purpose. A file served back from the app's own origin runs with the app's
// cookies if the browser decides to treat it as a page, so the dangerous types are not
// "denied", they are simply absent: HTML, SVG, XML and script are all documents a browser
// executes. Everything not listed is refused with ErrTypeNotAllowed.
//
// Inline is true only for the raster images a Markdown document embeds. Everything else is
// downloaded with Content-Disposition: attachment.
var allowed = map[string]struct{ Inline bool }{
	"image/png":  {Inline: true},
	"image/jpeg": {Inline: true},
	"image/gif":  {Inline: true},
	"image/webp": {Inline: true},

	"application/pdf":          {},
	"text/plain":               {},
	"text/csv":                 {},
	"text/markdown":            {},
	"application/json":         {},
	"application/zip":          {},
	"application/gzip":         {},
	"application/octet-stream": {},

	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {},
}

// Allowed reports whether a stored content type is one the API serves, and Inline whether
// it may be shown in place rather than downloaded.
func Allowed(contentType string) bool { _, ok := allowed[contentType]; return ok }

// Inline reports whether a stored content type is safe to render in the page.
func Inline(contentType string) bool { return allowed[contentType].Inline }

// textual types a client may name for content that sniffs as plain text; Go's sniffer
// cannot tell Markdown, CSV or JSON from any other text.
var textual = map[string]bool{
	"text/plain": true, "text/csv": true, "text/markdown": true, "application/json": true,
}

var openXML = map[string]bool{
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
}

// resolveType decides what an upload is, from its own leading bytes first.
//
// The client's declared type is a hint, never the answer: it is honoured only where
// sniffing cannot tell (text flavours, Office files that are zips) and only if it is on
// the allowlist. A declared type that is not on it - image/svg+xml, text/html - is
// refused outright even when the bytes look harmless, so what was said and what is
// stored cannot disagree about danger.
func resolveType(declared string, head []byte) (string, error) {
	d := baseType(declared)
	if d != "" && !Allowed(d) {
		return "", ErrTypeNotAllowed
	}

	s := baseType(http.DetectContentType(head))

	switch {
	case s == "text/plain":
		if textual[d] {
			return d, nil
		}

		return "text/plain", nil
	case s == "application/zip" && openXML[d]:
		return d, nil
	case Allowed(s):
		return s, nil
	}

	// text/html, text/xml (which is how an <?xml ... <svg> file sniffs), video and audio.
	return "", ErrTypeNotAllowed
}

// baseType is a media type without its parameters, lower case; "" when unparseable.
func baseType(v string) string {
	if v == "" {
		return ""
	}
	m, _, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}

	return strings.ToLower(m)
}

// MaxFilenameBytes bounds the stored name.
const MaxFilenameBytes = 255

// SanitizeFilename turns what a client sent into a name safe to store and to put in a
// header: the last path element (either separator), no control characters, no leading or
// trailing dots and spaces, at most MaxFilenameBytes, never empty.
//
// It is for display and for Content-Disposition only. The file is stored under a
// generated key, so nothing here is what keeps a path out of the filesystem - it is a
// second layer, not the first.
func SanitizeFilename(name string) string {
	name = strings.ToValidUTF8(name, "")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}

	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' || r == '‮' || r == '‎' || r == '‏' {
			return -1
		}

		return r
	}, name)
	name = strings.Trim(name, " .")

	for len(name) > MaxFilenameBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	name = strings.Trim(name, " .")

	if name == "" {
		return "file"
	}

	return name
}
