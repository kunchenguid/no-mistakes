package steps

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// Published PR text must be a fixed point of the read path. `gh pr view --json`
// (go-gh's asciisanitizer) rewrites C0 and C1 control characters, and the
// six-character JSON escapes that decode to them, into caret notation. The
// pr.template integrity checks compare the body gh reads back with the body that
// was sent, and the appendix digest is stamped over the sent bytes, so any such
// byte makes both fail with a false "edited" refusal. neutralizePRControls runs
// at the publication boundary, before the digest is stamped.

var (
	// ansiSequencePattern matches whole terminal escape sequences: CSI, OSC
	// (terminated by BEL or ST) and two-byte escapes. They are dropped rather
	// than rewritten so captured output stays readable.
	ansiSequencePattern = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)
	// jsonControlEscapePattern matches the literal text `\u001b` (any case) that
	// gh also rewrites, because it cannot tell it from an encoded control byte.
	// Captured JSON output carries it routinely.
	jsonControlEscapePattern = regexp.MustCompile(`(?i)\\u00(0[0-8cef]|1[0-9a-f]|[89][0-9a-f])`)
)

// neutralizePRControls removes terminal escape sequences and replaces every
// other control character gh would rewrite with U+FFFD. Tab, newline, vertical
// tab and carriage return are untouched. Literal `\u00XX` escapes of the
// rewritten characters become `\u{00XX}`. It is idempotent.
func neutralizePRControls(s string) string {
	s = ansiSequencePattern.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if rewrittenByGH(r) {
			return '�'
		}
		return r
	}, s)
	return jsonControlEscapePattern.ReplaceAllStringFunc(s, func(m string) string {
		return `\u{` + m[2:] + `}`
	})
}

func rewrittenByGH(r rune) bool {
	switch {
	case r == '\t', r == '\n', r == '\v', r == '\r':
		return false
	case r < 0x20:
		return true
	}
	return r >= 0x80 && r <= 0x9f
}

// maxCaretRecoveryPairs bounds the subset search in appendixDigestMatches. Each
// extra caret pair doubles the candidates hashed (2^12 candidates of at most one
// PR body is a fraction of a second).
const maxCaretRecoveryPairs = 12

// caretControlPositions lists the offsets of the carets that start a pair gh
// could have produced from a C0 control character.
func caretControlPositions(s string) []int {
	var at []int
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '^' {
			continue
		}
		if c := s[i+1]; c >= '@' && c <= '_' && c != 'I' && c != 'J' && c != 'K' && c != 'M' {
			at = append(at, i)
			i++
		}
	}
	return at
}

// restoreCaretControlsAt maps the caret pairs at the offsets for which restore
// returns true (called with the index k into at) back to the C0 control
// characters gh produced them from.
func restoreCaretControlsAt(s string, at []int, restore func(k int) bool) string {
	var b strings.Builder
	prev := 0
	for k, i := range at {
		if !restore(k) {
			continue
		}
		b.WriteString(s[prev:i])
		b.WriteByte(s[i+1] - '@')
		prev = i + 2
	}
	b.WriteString(s[prev:])
	return b.String()
}

// restoreCaretControls maps every caret pair back to the C0 control character
// gh produces it from, so an appendix that an earlier version stamped over raw
// control bytes can still be recognised as ours. C1 characters share their
// caret form with C0 and are restored as C0.
func restoreCaretControls(s string) string {
	at := caretControlPositions(s)
	return restoreCaretControlsAt(s, at, restoreAll)
}

func restoreAll(int) bool { return true }

// appendixDigestMatches reports whether appendix carries the stamped digest,
// either byte for byte or after undoing gh's caret rewriting of control bytes
// that an earlier version embedded. A caret pair may be a rewritten control
// byte or text that was always there (`grep '^ERROR'`), so the digest decides
// which ones to restore: every subset is tried, never a guess.
func appendixDigestMatches(appendix, digest string) bool {
	matches := func(s string) bool { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) == digest }
	if matches(appendix) {
		return true
	}
	at := caretControlPositions(appendix)
	if len(at) == 0 {
		return false
	}
	if len(at) > maxCaretRecoveryPairs {
		// shortcut: beyond the cap only the all-restored reading is tried; raise the cap if legacy PRs mixing that many literal carets turn up.
		return matches(restoreCaretControlsAt(appendix, at, restoreAll))
	}
	for mask := uint64(1); mask < 1<<uint(len(at)); mask++ {
		if matches(restoreCaretControlsAt(appendix, at, func(k int) bool { return mask&(1<<uint(k)) != 0 })) {
			return true
		}
	}
	return false
}
