// Package safeoutput escapes control characters in untrusted terminal output.
package safeoutput

import (
	"strconv"
	"strings"
	"unicode"
)

// Escape makes text safe to print on one terminal line without changing the
// underlying error value or its wrapping chain.
func Escape(s string) string {
	var out strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			out.WriteRune(r)
		} else {
			q := strconv.QuoteRune(r)
			out.WriteString(q[1 : len(q)-1])
		}
	}
	return out.String()
}
