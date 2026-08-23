// Package safepath keeps operator filesystem identity - home directory paths -
// out of the text a caller passes it.
//
// It guarantees nothing on its own about what no-mistakes publishes: coverage
// is exactly the set of publication boundaries that call it. Today that is the
// pull request title and body (PRStep.buildPRContent). Agent-authored commit
// subjects, which reach the remote through the auto-fix commit path
// (commitAgentFixes -> Commit.RenderFixMessage), are a separate surface with a
// different rendering and are deliberately not covered; so is the opt-in
// evidence branch, which copies artifact files verbatim.
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
// The leading group is what precedes the match, re-emitted unchanged: a single
// boundary byte - which may be the ':' of a colon-separated list - or a
// one-letter flag prefix such as "-I" glued to the path, or the two bytes of a
// JSON string escape (\n, \r, \t), which is what puts a home path at the start
// of a line inside JSON-escaped captured output. It exists because RE2 has no
// lookbehind and the alternative - allowing any preceding bytes - would
// rewrite the "/users/" segment of an ordinary URL such as
// https://api.github.com/users/octocat. "file://" is spelled out for the one
// case where a slash legitimately precedes the home root.
//
// Each separator position also accepts the doubled "\\" that JSON-escaped
// output prints (C:\\Users\<user>), because captured output routinely embeds
// JSON. A doubled FORWARD slash is deliberately not a separator: "//" after a
// scheme colon is a URL authority, so accepting it would rewrite
// https://users/list as https:~, and leaving the rare //home/<user> spelling
// intact costs less than mangling every such URL.
var genericHomePattern = regexp.MustCompile(
	`(^|-[A-Za-z]|\\[nrt]|[^A-Za-z0-9_./\\-])((?i:file://)?(?:[A-Za-z]:)?(?:\\\\|[/\\])(?i:home|users)(?:\\\\|[/\\])[^/\\\s"'` + "`" + `<>()\[\]{},;:&|*?]+)`)

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

// addBothSeparatorSpellings registers a candidate written with each separator,
// plus the doubled-backslash spelling JSON-escaped output prints. Captured
// output mixes them on Windows, and it also removes the last
// platform-dependent step from candidate resolution: filepath.Clean rewrites
// separators for the platform the binary was built for, so without this the
// candidate set - and therefore what gets redacted - would differ between a
// Linux test run and a Windows one.
func addBothSeparatorSpellings(add func(string), home string) {
	add(home)
	add(strings.ReplaceAll(home, `\`, "/"))
	add(strings.ReplaceAll(home, "/", `\`))
	add(strings.ReplaceAll(home, `\`, `\\`))
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
	specific := specificHomeCandidate(home)
	var b strings.Builder
	for i := 0; i < len(text); {
		offset := strings.Index(text[i:], home)
		if offset < 0 {
			b.WriteString(text[i:])
			return b.String()
		}
		start := i + offset
		end := start + len(home)
		if boundaryBefore(text, start, specific) && boundaryAfter(text, end) {
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

// specificHomeCandidate reports whether home names at least two path segments
// beneath its root (/home/<user>, /srv/<name>, C:\Users\<user>). Only a
// candidate that specific may match inside a URL, where the byte before the
// home is the tail of the host or of a previous path segment; a
// single-segment home such as /data would instead rewrite that segment of an
// unrelated URL (https://cdn.example.com/data/file.json).
func specificHomeCandidate(home string) bool {
	rest, _ := trimAbsoluteRoot(home)
	return strings.ContainsAny(rest, `/\`)
}

// boundaryBefore reports whether position i in text starts a path rather than
// the middle of a longer name. specific carries the caller's candidate
// specificity, which is what admits a name-byte boundary inside a URL.
func boundaryBefore(text string, i int, specific bool) bool {
	if i == 0 {
		return true
	}
	switch c := text[i-1]; {
	case c == '/':
		// The one slash that legitimately precedes an absolute path.
		return strings.HasSuffix(text[:i], "://")
	case c == '\\' || isPathNameByte(c):
		// The letter of a JSON string escape (\n, \r, \t) is the tail of an
		// escape sequence, not a name byte glued to the path - the shape
		// JSON-escaped captured output prints for a line-initial home path.
		if i >= 2 && text[i-2] == '\\' && (c == 'n' || c == 'r' || c == 't') {
			return true
		}
		if i >= 2 && text[i-2] == '-' && isASCIILetter(c) {
			return true
		}
		// Inside a URL that byte is the tail of the host or the previous path
		// segment (https://host/evidence/home/<user>/x.log). Outside one, the
		// same shape is a different path the home is merely a suffix of
		// (/opt/srv/<name>), so it stays a non-boundary.
		return specific && insideURL(text, i)
	default:
		return true
	}
}

// insideURL reports whether position i in text sits inside a URL: a scheme
// separator ("://") appears earlier in the text with no whitespace between it
// and i, so the bytes before i belong to a host or path segment rather than
// to an ordinary token.
func insideURL(text string, i int) bool {
	prefix := text[:i]
	scheme := strings.LastIndex(prefix, "://")
	return scheme >= 0 && !strings.ContainsAny(prefix[scheme:], " \t\r\n")
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
