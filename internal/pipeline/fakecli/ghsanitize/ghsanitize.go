// Package ghsanitize is a test-only port of the response sanitizer that the
// GitHub CLI's API client (github.com/cli/go-gh/v2/pkg/asciisanitizer, used with
// JSON enabled) runs over every API response, including `gh pr view --json`.
// It rewrites C0 and C1 control characters, and the six-character JSON escapes
// that decode to them, into caret notation, so a body read back through gh is
// not byte-for-byte what was written.
//
// The fake gh binary and the publication tests share this port so a body that
// survives it round-trips through the real client too.
package ghsanitize

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// JSON sanitizes a JSON document exactly as asciisanitizer.Sanitizer{JSON: true}
// does when it has the whole response.
func JSON(src []byte) []byte {
	var out bytes.Buffer
	addEscape := false
	for len(src) > 0 {
		r, size := utf8.DecodeRune(src)
		if r == utf8.RuneError && size < 2 {
			// The real sanitizer fails the read on invalid UTF-8.
			out.Write(src[:size])
			src = src[size:]
			continue
		}
		if unicode.IsControl(r) {
			if repl, ok := caret(r); ok {
				out.WriteString(repl)
				src = src[size:]
				continue
			}
		}
		if len(src) >= 6 {
			if repl, ok := jsonCaret(src[:6]); ok {
				if addEscape {
					repl = "\\" + repl
					addEscape = false
				}
				out.WriteString(repl)
				src = src[6:]
				continue
			}
		}
		out.Write(src[:size])
		src = src[size:]
		addEscape = r == '\\' && !addEscape
	}
	return out.Bytes()
}

// Text returns what gh hands back for a string stored on the server: the text
// is encoded as a JSON string, sanitized, and decoded again.
func Text(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return s
	}
	var out string
	if err := json.Unmarshal(JSON(encoded), &out); err != nil {
		return s
	}
	return out
}

func sanitized(code int) bool {
	switch {
	case code == 9, code == 10, code == 11, code == 13:
		return false
	case code < 32:
		return true
	case code >= 0x80 && code <= 0x9f:
		return true
	}
	return false
}

// caretFor mirrors the library's table: the C0 code point c maps to '@'+c, and
// the C1 code point 0x80+c maps to the same letter. Code 28 is the library's
// three-character `^\\`.
func caretFor(code int) string {
	if code >= 0x80 {
		code -= 0x80
	}
	if code == 28 {
		return `^\\`
	}
	return "^" + string(rune('@'+code))
}

func caret(r rune) (string, bool) {
	if !sanitized(int(r)) {
		return "", false
	}
	return caretFor(int(r)), true
}

func jsonCaret(b []byte) (string, bool) {
	if len(b) != 6 || !bytes.HasPrefix(b, []byte(`\u00`)) {
		return "", false
	}
	var code int
	for _, c := range strings.ToLower(string(b[4:])) {
		switch {
		case c >= '0' && c <= '9':
			code = code*16 + int(c-'0')
		case c >= 'a' && c <= 'f':
			code = code*16 + int(c-'a') + 10
		default:
			return "", false
		}
	}
	if !sanitized(code) {
		return "", false
	}
	return caretFor(code), true
}
