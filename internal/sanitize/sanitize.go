// Package sanitize turns WhatsApp-supplied strings into single feed lines.
package sanitize

import (
	"strings"
	"unicode"
)

const breakMarker = " / "

// Line makes s safe to place on one line of the feed. Line, paragraph and
// NEL separators become " / " (a CRLF pair counts as one break), tabs become
// a space, and other control characters are dropped. Format characters such
// as U+200B are kept.
func Line(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\r' && i+1 < len(rs) && rs[i+1] == '\n':
			i++
			b.WriteString(breakMarker)
		case r == '\n', r == '\r', r == '\u0085', r == '\u2028', r == '\u2029':
			b.WriteString(breakMarker)
		case r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
