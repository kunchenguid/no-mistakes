// Package safepath keeps operator filesystem identity - home directory paths -
// out of text no-mistakes is about to publish.
//
// It is the path analogue of internal/safeurl: safeurl keeps credentials out of
// published text, safepath keeps the operator's home directory out of it. Route
// new publication surfaces through this package instead of adding a local
// path-scrubbing helper, so there is one owner of the rules and one place a new
// shape has to be taught.
//
// The rules are deliberately blunt. A false positive costs a slightly uglier
// PR body; a false negative publishes someone's username to the internet.
package safepath

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Placeholder replaces every redacted home directory prefix.
//
// "~" is chosen over an angle-bracketed token on purpose: the PR body is
// assembled with HTML escaping applied before this boundary runs, so inserting
// a literal "<home>" afterwards would be re-read as an unknown HTML tag by
// GitHub's markdown renderer and silently disappear. "~" carries no markdown or
// HTML meaning, and it is never longer than what it replaces, so redaction can
// only shrink an already length-capped body.
const Placeholder = "~"

// genericHomePattern matches the conventional home roots regardless of which
// account the daemon runs as: /home/<user>, /Users/<user>, and their Windows
// (C:\Users\<user>) and file:// URL spellings. Only the "<user>" segment is
// consumed, so the rest of the path survives and the result still reads as a
// path.
//
// The leading group is the byte before the match, re-emitted unchanged. It
// exists because RE2 has no lookbehind and the alternative - allowing any
// preceding byte - would rewrite the "/users/" segment of an ordinary URL such
// as https://api.github.com/users/octocat. "file://" is spelled out for the one
// case where a slash legitimately precedes the home root.
var genericHomePattern = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.:/\\-])((?i:file://)?(?:[A-Za-z]:)?[/\\](?i:home|users)[/\\][^/\\\s"'` + "`" + `<>()\[\]{},;:&|*?]+)`)

// RedactText replaces every absolute home directory path in text with
// Placeholder. It is unconditional: there is no detect-and-warn mode, because
// the only safe default at a publication boundary is that the path is already
// gone by the time anyone reads the output.
//
// Callers may pass an entire assembled document. Redaction is applied to every
// occurrence, not just the first: an earlier consumer-side guard reported only
// one hit per body and let repeats through.
func RedactText(text string) string {
	if text == "" {
		return text
	}
	// The account's own home first, because it may sit outside the
	// conventional roots (/root, a container path, a relocated home), then the
	// conventional roots for every other account.
	for _, home := range homeCandidates() {
		text = replaceHomePrefix(text, home)
	}
	return genericHomePattern.ReplaceAllString(text, "${1}"+Placeholder)
}

// homeCandidates lists the spellings this process's own home directory can
// appear as, longest first so a nested candidate never shadows its parent.
// Every lookup failure is skipped rather than reported: RedactText still has
// the conventional-root rules, and a redactor that can error is a redactor
// somebody eventually calls without checking.
func homeCandidates() []string {
	var raw []string
	if home, err := os.UserHomeDir(); err == nil {
		raw = append(raw, home)
	}
	raw = append(raw, os.Getenv("HOME"), os.Getenv("USERPROFILE"))

	seen := map[string]bool{}
	var out []string
	add := func(candidate string) {
		if !usableHomeCandidate(candidate) || seen[candidate] {
			return
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	for _, home := range raw {
		home = filepath.Clean(strings.TrimSpace(home))
		addBothSeparatorSpellings(add, home)
		// A macOS home reached through /var reads back as /private/var, and a
		// symlinked home directory reads back as its target. Captured output
		// can carry either spelling.
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			addBothSeparatorSpellings(add, filepath.Clean(resolved))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// addBothSeparatorSpellings registers a candidate written with each separator.
// Captured output mixes them on Windows, and it also removes the last
// platform-dependent step from candidate resolution: filepath.Clean rewrites
// separators for the platform the binary was built for, so without this the
// candidate set - and therefore what gets redacted - would differ between a
// Linux test run and a Windows one.
func addBothSeparatorSpellings(add func(string), home string) {
	add(home)
	add(strings.ReplaceAll(home, `\`, "/"))
	add(strings.ReplaceAll(home, "/", `\`))
}

// usableHomeCandidate rejects values too short or too broad to be a home
// directory. Redacting "/" or a bare volume root would rewrite every path in
// the document, which is over-redaction past the point of usefulness.
//
// Absoluteness is judged in both platforms' spellings rather than with
// filepath.IsAbs, which answers only for the platform the binary was built
// for. On Windows, filepath.IsAbs rejects a POSIX-rooted HOME - the spelling
// Git Bash, MSYS2, and Cygwin set (/c/Users/<user>, /home/<user>) and the one
// their captured output prints - so an IsAbs gate silently disables redaction
// for exactly the shell setup a Windows developer is most likely to run tests
// under. A home this function rejects is a home that never gets redacted, so
// it errs toward accepting.
func usableHomeCandidate(home string) bool {
	if len(home) < 4 {
		return false
	}
	rest, absolute := trimAbsoluteRoot(home)
	if !absolute {
		return false
	}
	// A bare root - "/", a drive letter, a UNC prefix - names no account, and
	// matching it would rewrite every path in the document.
	return strings.Trim(rest, `/\.`) != ""
}

// trimAbsoluteRoot strips a leading Windows drive, UNC, or POSIX root and
// reports whether the value was rooted in either spelling.
func trimAbsoluteRoot(p string) (rest string, absolute bool) {
	if len(p) > 2 && p[1] == ':' && isASCIILetter(p[0]) && (p[2] == '/' || p[2] == '\\') {
		return strings.TrimLeft(p[2:], `/\`), true
	}
	if p != "" && (p[0] == '/' || p[0] == '\\') {
		return strings.TrimLeft(p, `/\`), true
	}
	return "", false
}

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// replaceHomePrefix rewrites every path-boundary-aligned occurrence of home.
// Boundary alignment is what keeps "/home/dev" from clipping "/home/developer"
// and what keeps an unrelated "/srv/home/dev" intact.
func replaceHomePrefix(text, home string) string {
	if home == "" || !strings.Contains(text, home) {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		offset := strings.Index(text[i:], home)
		if offset < 0 {
			b.WriteString(text[i:])
			return b.String()
		}
		start := i + offset
		end := start + len(home)
		if boundaryBefore(text, start) && boundaryAfter(text, end) {
			b.WriteString(text[i:start])
			b.WriteString(Placeholder)
			i = end
			continue
		}
		b.WriteString(text[i : start+1])
		i = start + 1
	}
	return b.String()
}

func boundaryBefore(text string, i int) bool {
	if i == 0 {
		return true
	}
	switch c := text[i-1]; {
	case c == '/':
		// The one slash that legitimately precedes an absolute path.
		return strings.HasSuffix(text[:i], "://")
	case c == '\\' || c == ':' || isPathNameByte(c):
		return false
	default:
		return true
	}
}

func boundaryAfter(text string, i int) bool {
	if i >= len(text) {
		return true
	}
	c := text[i]
	return c == '/' || c == '\\' || !isPathNameByte(c)
}

// isPathNameByte reports whether c can appear inside a single path segment
// name. A home directory followed by one of these is a different directory
// whose name merely starts the same way.
func isPathNameByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '.' || c == '-':
		return true
	default:
		return false
	}
}
