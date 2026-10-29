package search

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PlainText reduces Markdown to the text a person reads (LAM-143). It is the one
// Markdown-to-plain-text function in this repo: everything that is indexed for search
// goes through it, so the snippet ts_headline cuts from the index never carries syntax.
//
//   - an image becomes "[image]"; a link becomes its label, whatever its URL scheme
//     (https, /api/..., lf://ticket|document|project/<id>)
//   - emphasis, strikethrough, heading, quote, list and fence markers are dropped; the
//     content of code spans and fenced blocks is kept as written
//   - anything else is left alone. Raw HTML and entities pass through untouched: the
//     read path escapes the snippet, so there is nothing to neutralise here, and
//     rewriting "a < b" as if it were a tag would lose text.
//
// It is deliberately a small scanner, not a CommonMark implementation: input that is
// not valid Markdown comes out as itself.
func PlainText(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	fence := ""

	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if marker := fenceMarker(trimmed); marker != "" {
			switch {
			case fence == "":
				fence = marker
				continue
			case strings.HasPrefix(marker, fence) && strings.Trim(trimmed, marker[:1]) == "":
				fence = ""
				continue
			}
		}
		if fence != "" {
			out = append(out, line)
			continue
		}
		if ruleLine.MatchString(trimmed) {
			continue
		}

		out = append(out, inline(stripBlock(trimmed)))
	}

	return strings.Join(out, "\n")
}

var (
	ruleLine = regexp.MustCompile(`^(?:-[ \t]*){3,}$|^(?:\*[ \t]*){3,}$|^(?:_[ \t]*){3,}$`)
	heading  = regexp.MustCompile(`^#{1,6}(?:[ \t]+|$)`)
	headEnd  = regexp.MustCompile(`[ \t]+#+[ \t]*$`)
	listItem = regexp.MustCompile(`^(?:[-*+]|\d{1,9}[.)])[ \t]+(?:\[[ xX]\][ \t]+)?`)
)

// fenceMarker returns the run of 3+ backticks or tildes that opens or closes a fence,
// or "" when the line is not a fence line.
func fenceMarker(line string) string {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return ""
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return ""
	}

	return line[:n]
}

// stripBlock removes the markers that begin a line: quotes, headings, list bullets.
func stripBlock(line string) string {
	for strings.HasPrefix(line, ">") {
		line = strings.TrimLeft(strings.TrimPrefix(line, ">"), " ")
	}
	if heading.MatchString(line) {
		line = strings.TrimLeft(line, "#")
		line = headEnd.ReplaceAllString(strings.TrimLeft(line, " \t"), "")

		return line
	}

	return listItem.ReplaceAllString(line, "")
}

// inline resolves code spans, images, links, escapes and emphasis markers in one line.
func inline(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && isPunct(s[i+1]):
			b.WriteByte(s[i+1])
			i += 2

		case c == '`':
			run := runLen(s, i, '`')
			if end := closingRun(s, i+run, run); end >= 0 {
				b.WriteString(s[i+run : end])
				i = end + run
			} else {
				b.WriteString(s[i : i+run])
				i += run
			}

		case c == '!' && i+1 < len(s) && s[i+1] == '[':
			if _, next, ok := link(s, i+1); ok {
				b.WriteString("[image]")
				i = next
			} else {
				b.WriteByte(c)
				i++
			}

		case c == '[':
			if label, next, ok := link(s, i); ok {
				b.WriteString(inline(label))
				i = next
			} else {
				b.WriteByte(c)
				i++
			}

		case c == '*' || c == '_' || c == '~':
			run := runLen(s, i, c)
			if isEmphasis(s, i, run) {
				i += run
			} else {
				b.WriteString(s[i : i+run])
				i += run
			}

		default:
			b.WriteByte(c)
			i++
		}
	}

	return b.String()
}

// link parses "[label](dest)" at s[at], where s[at] is '['. Brackets inside the label
// nest, so "[a [b] c](u)" has the label "a [b] c"; a backslash escapes either bracket.
// next is the index after the closing ")".
func link(s string, at int) (label string, next int, ok bool) {
	depth := 0
	i := at
scan:
	for ; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '`':
			// A code span hides brackets, as in CommonMark.
			run := runLen(s, i, '`')
			if end := closingRun(s, i+run, run); end >= 0 {
				i = end + run - 1
			} else {
				i += run - 1
			}
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				break scan
			}
		}
	}
	if i >= len(s) || i+1 >= len(s) || s[i+1] != '(' {
		return "", 0, false
	}

	// The destination runs to the matching ")": parentheses in a URL nest, and a
	// title after a space ("url \"t\"") is part of it.
	parens := 0
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '(':
			parens++
		case ')':
			parens--
			if parens == 0 {
				return s[at+1 : i], j + 1, true
			}
		}
	}

	return "", 0, false
}

// isEmphasis reports whether the run of c at s[i:i+run] is an emphasis marker rather
// than text: a lone "*" in "2 * 3" and the underscores of "snake_case" are text.
func isEmphasis(s string, i, run int) bool {
	if c := s[i]; c == '~' && run < 2 {
		return false
	}
	before, _ := utf8.DecodeLastRuneInString(s[:i])
	after, _ := utf8.DecodeRuneInString(s[i+run:])
	if i == 0 {
		before = ' '
	}
	if i+run >= len(s) {
		after = ' '
	}
	if unicode.IsSpace(before) && unicode.IsSpace(after) {
		return false
	}
	if s[i] == '_' && isWord(before) && isWord(after) {
		return false
	}

	return true
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func isPunct(c byte) bool { return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0 }

func runLen(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}

	return n
}

// closingRun finds the next run of exactly n backticks at or after from, or -1.
func closingRun(s string, from, n int) int {
	for i := from; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		run := runLen(s, i, '`')
		if run == n {
			return i
		}
		i += run
	}

	return -1
}
