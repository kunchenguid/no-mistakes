// Package safecontent removes local execution identity from generated content
// before it is handed to a publication transport.
package safecontent

import (
	"errors"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/safepath"
)

const (
	// Placeholders are Markdown-safe and no longer than the concrete values
	// they replace. Keeping replacements non-growing preserves PR body limits
	// already applied by renderers.
	TreehousePlaceholder = "[worktree]"
	TempPlaceholder      = "[temp]"
	SessionPlaceholder   = "[id]"
)

// ErrUnsafePullRequestContent is returned when content names a local-data
// shape that cannot be transformed with confidence. It deliberately carries
// none of the rejected text so a caller can log it safely.
var ErrUnsafePullRequestContent = errors.New("pull request content did not pass local-data privacy validation")

const pathTokenTerminators = `\s"'` + "`" + `<>()\[\]{},;&|*?`

var (
	treehousePathPattern = regexp.MustCompile(
		`(?i)(^|-[A-Za-z]|\\[nrt]|[^A-Za-z0-9_./\\~-])((?:file://)?(?:~|(?:[A-Za-z]:)?[/\\])[^` + pathTokenTerminators + `]*[/\\]\.treehouse[/\\][^` + pathTokenTerminators + `]+)`)
	macTempPathPattern = regexp.MustCompile(
		`(^|-[A-Za-z]|\\[nrt]|[^A-Za-z0-9_./\\-])((?:/private)?/var/folders/[^/\s]+/[^/\s]+/[TC](?:/[^` + pathTokenTerminators + `]+)?)`)
	unixTempPathPattern = regexp.MustCompile(
		`(^|-[A-Za-z]|\\[nrt]|[^A-Za-z0-9_./\\-])((?:/private)?/(?:var/tmp|tmp)/[^` + pathTokenTerminators + `]+)`)

	sessionAssignmentPattern = regexp.MustCompile(
		`(?i)(\b(?:PI_SESSION_ID|HERDR_(?:SESSION|RUN|PANE|TAB|WORKSPACE)_ID)\b["']?\s*[:=]\s*["'` + "`" + `]?)([A-Za-z0-9][A-Za-z0-9._:/-]*)`)
	contextualSessionPattern = regexp.MustCompile(
		`(?i)(\b(?:(?:pi|herdr)[ -](?:session|run|pane|tab|workspace)|run|(?:run|session)[ _-]?id)\b(?:\s*[:=])?\s*["'` + "`" + `]?)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9A-HJKMNP-TV-Z]{26}|[A-Za-z]{1,12}:[A-Za-z0-9_-]{1,20})`)
	uuidPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	ulidPattern = regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{26}\b`)
)

// ScrubPullRequestText removes concrete local paths and harness session
// identities from one complete pull-request field. Known shapes are replaced
// rather than dropping surrounding evidence. Ambiguous Treehouse paths fail
// closed, because publishing a partially understood managed-copy name would
// defeat the boundary.
func ScrubPullRequestText(text string) (string, error) {
	if text == "" {
		return text, nil
	}

	text = safepath.RedactText(text)

	var uncertainTreehousePath bool
	text = treehousePathPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := treehousePathPattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			uncertainTreehousePath = true
			return match
		}
		if parts[1] == ":" && strings.HasPrefix(parts[2], "//") {
			// The regexp can see the slash pair after an HTTP scheme as a
			// rooted path. Keep network URLs byte-stable.
			return match
		}
		redacted, ok := redactTreehousePath(parts[2])
		if !ok {
			uncertainTreehousePath = true
			return match
		}
		return parts[1] + redacted
	})
	if uncertainTreehousePath {
		return "", ErrUnsafePullRequestContent
	}

	text = redactCurrentTempDir(text)
	text = macTempPathPattern.ReplaceAllString(text, "${1}"+TempPlaceholder)
	text = unixTempPathPattern.ReplaceAllStringFunc(text, redactGeneratedUnixTempPath)

	for _, id := range currentSessionIDs() {
		text = strings.ReplaceAll(text, id, SessionPlaceholder)
	}
	text = sessionAssignmentPattern.ReplaceAllStringFunc(text, func(match string) string {
		return redactSessionMatch(sessionAssignmentPattern, match)
	})
	text = contextualSessionPattern.ReplaceAllStringFunc(text, func(match string) string {
		return redactSessionMatch(contextualSessionPattern, match)
	})
	text = redactIDsInLocalStatePaths(text)

	return text, nil
}

func redactTreehousePath(path string) (string, bool) {
	if strings.Contains(path, "://") && !strings.HasPrefix(strings.ToLower(path), "file://") {
		// Ordinary URLs are publication-safe evidence, not local paths. A
		// file:// URL is local and is handled below.
		return path, true
	}
	normalized := strings.TrimPrefix(path, "file://")
	normalized = strings.ReplaceAll(normalized, `\\`, "/")
	segments := strings.FieldsFunc(normalized, func(r rune) bool { return r == '/' || r == '\\' })
	marker := -1
	for i, segment := range segments {
		if strings.EqualFold(segment, ".treehouse") {
			marker = i
			break
		}
	}
	if marker < 0 || len(segments) <= marker+2 {
		return "", false
	}
	project := segments[marker+1]
	slot := segments[marker+2]
	if _, err := strconv.ParseUint(slot, 10, 32); err != nil {
		return "", false
	}

	remainder := segments[marker+3:]
	if len(remainder) > 0 && strings.EqualFold(remainder[0], treehouseProjectName(project)) {
		remainder = remainder[1:]
	}
	if len(remainder) == 0 {
		return TreehousePlaceholder, true
	}
	return TreehousePlaceholder + "/" + strings.Join(remainder, "/"), true
}

func treehouseProjectName(managed string) string {
	cut := strings.LastIndexByte(managed, '-')
	if cut <= 0 {
		return managed
	}
	suffix := managed[cut+1:]
	if len(suffix) < 6 {
		return managed
	}
	for _, r := range suffix {
		if !isASCIIAlphaNumeric(r) {
			return managed
		}
	}
	return managed[:cut]
}

func isASCIIAlphaNumeric(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

func redactCurrentTempDir(text string) string {
	root := strings.TrimRight(strings.TrimSpace(os.TempDir()), `/\\`)
	if root == "" || root == "/tmp" || root == "/private/tmp" || root == "/var/tmp" {
		return text
	}
	spellings := []string{
		root,
		strings.ReplaceAll(root, `\`, "/"),
		strings.ReplaceAll(root, "/", `\`),
		strings.ReplaceAll(root, `\`, `\\`),
	}
	sort.SliceStable(spellings, func(i, j int) bool { return len(spellings[i]) > len(spellings[j]) })
	for _, spelling := range spellings {
		if spelling == "" || !strings.Contains(text, spelling) {
			continue
		}
		pattern := regexp.MustCompile(`(^|-[A-Za-z]|\\[nrt]|[^A-Za-z0-9_./\\-])` + regexp.QuoteMeta(spelling) + `(?:[/\\][^` + pathTokenTerminators + `]+)?`)
		text = pattern.ReplaceAllString(text, "${1}"+TempPlaceholder)
	}
	return text
}

func redactGeneratedUnixTempPath(match string) string {
	parts := unixTempPathPattern.FindStringSubmatch(match)
	if len(parts) != 3 || !isGeneratedUnixTempPath(parts[2]) {
		return match
	}
	return parts[1] + TempPlaceholder
}

func isGeneratedUnixTempPath(path string) bool {
	normalized := strings.TrimPrefix(path, "/private")
	var rest string
	switch {
	case strings.HasPrefix(normalized, "/tmp/"):
		rest = strings.TrimPrefix(normalized, "/tmp/")
	case strings.HasPrefix(normalized, "/var/tmp/"):
		rest = strings.TrimPrefix(normalized, "/var/tmp/")
	default:
		return false
	}
	first, after, _ := strings.Cut(rest, "/")
	lower := strings.ToLower(first)
	switch {
	case strings.HasPrefix(lower, "pytest-of-"),
		strings.HasPrefix(lower, "go-build"),
		strings.HasPrefix(lower, "pi-bash-"),
		strings.HasPrefix(lower, "pi-read-"),
		strings.HasPrefix(lower, "pi-write-"),
		strings.HasPrefix(lower, "pi-edit-"),
		looksLikeMktempName(first),
		looksLikeGoTestTempName(first),
		uuidPattern.MatchString(first),
		ulidPattern.MatchString(first):
		return true
	case lower == "no-mistakes-evidence":
		second, _, _ := strings.Cut(after, "/")
		return uuidPattern.MatchString(second) || ulidPattern.MatchString(second)
	default:
		return false
	}
}

func looksLikeMktempName(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{"tmp.", "tmp-"} {
		if strings.HasPrefix(lower, prefix) && len(name) >= len(prefix)+6 {
			return true
		}
	}
	return false
}

func looksLikeGoTestTempName(name string) bool {
	if !strings.HasPrefix(name, "Test") || len(name) < 10 {
		return false
	}
	digits := 0
	for i := len(name) - 1; i >= 0 && name[i] >= '0' && name[i] <= '9'; i-- {
		digits++
	}
	return digits >= 5
}

func redactSessionMatch(pattern *regexp.Regexp, match string) string {
	parts := pattern.FindStringSubmatch(match)
	if len(parts) != 3 {
		return match
	}
	if pattern == contextualSessionPattern && bareRunContext(parts[1]) && !ulidPattern.MatchString(parts[2]) {
		// "run package:target" and similar command documentation is not a
		// session reference. Bare Run is accepted only for the ULID-shaped
		// local run identity no-mistakes and Herdr emit.
		return match
	}
	placeholder := SessionPlaceholder
	if len(parts[2]) < len(placeholder) {
		// Keep the connector's no-growth guarantee even for unusually short
		// labeled Herdr workspace aliases.
		placeholder = strings.Repeat("?", len(parts[2]))
	}
	return parts[1] + placeholder
}

func bareRunContext(prefix string) bool {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	prefix = strings.TrimSpace(strings.TrimRight(prefix, "`\"'"))
	return prefix == "run"
}

func currentSessionIDs() []string {
	keys := []string{
		"PI_SESSION_ID",
		"HERDR_SESSION_ID",
		"HERDR_RUN_ID",
		"HERDR_PANE_ID",
		"HERDR_TAB_ID",
		"HERDR_WORKSPACE_ID",
	}
	seen := map[string]bool{}
	var ids []string
	for _, key := range keys {
		id := strings.TrimSpace(os.Getenv(key))
		// Very short ambient values (for example a two-letter workspace
		// alias) are unsafe to replace globally. A labeled assignment is
		// still covered by sessionAssignmentPattern.
		if len(id) < len(SessionPlaceholder) || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool { return len(ids[i]) > len(ids[j]) })
	return ids
}

func redactIDsInLocalStatePaths(text string) string {
	// Home redaction intentionally leaves the useful path below ~ intact. IDs
	// inside known local state trees are not useful public evidence, however.
	for _, marker := range []string{
		"~/.pi/", `~\.pi\`, `~\\.pi\\`,
		"~/.herdr/", `~\.herdr\`, `~\\.herdr\\`,
		"~/.no-mistakes/evidence/", `~\.no-mistakes\evidence\`, `~\\.no-mistakes\\evidence\\`,
	} {
		for offset := 0; ; {
			rel := strings.Index(text[offset:], marker)
			if rel < 0 {
				break
			}
			start := offset + rel
			end := start + len(marker)
			for end < len(text) && !strings.ContainsRune(" \t\r\n\"'`<>()[]{},;&|*?", rune(text[end])) {
				end++
			}
			path := text[start:end]
			redacted := uuidPattern.ReplaceAllString(path, SessionPlaceholder)
			redacted = ulidPattern.ReplaceAllString(redacted, SessionPlaceholder)
			text = text[:start] + redacted + text[end:]
			offset = start + len(redacted)
		}
	}
	return text
}
