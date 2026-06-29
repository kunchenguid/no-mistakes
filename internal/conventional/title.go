package conventional

import (
	"regexp"
	"strings"
)

var titleRe = regexp.MustCompile(`^([a-z]+)(\([^)]+\))?(!)?: (.+)$`)

var validTypes = map[string]bool{
	"feat":     true,
	"fix":      true,
	"docs":     true,
	"style":    true,
	"refactor": true,
	"perf":     true,
	"test":     true,
	"build":    true,
	"ci":       true,
	"chore":    true,
	"revert":   true,
}

const ReleaseTypeRule = `- If the change has any user-facing product impact, the type must use feat or fix so release automation can pick it up. Use feat for a new user-visible capability and fix for a user-visible correction or behavior improvement. Use docs, refactor, chore, test, build, or ci only when the change has no user-facing product behavior impact.`

func IsTitle(title string) bool {
	m := titleRe.FindStringSubmatch(strings.TrimSpace(title))
	return len(m) > 0 && validTypes[m[1]]
}

// ExtractTicket returns the first substring of branch matching pattern, used to
// pull a work-item id (e.g. "WEB-12345") out of a branch name. A blank or
// invalid pattern, or no match, yields "".
func ExtractTicket(branch, pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ""
	}
	return re.FindString(strings.TrimSpace(branch))
}

// ApplyTicketPrefix returns "ticket: description", where description is title
// with any leading conventional "type(scope)!: " prefix stripped so the result
// is not double-prefixed (e.g. "docs: refresh readme" -> "WEB-1: refresh
// readme"). A blank ticket returns title unchanged; a title that already starts
// with the ticket is returned as-is.
func ApplyTicketPrefix(title, ticket string) string {
	title = strings.TrimSpace(title)
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return title
	}
	if strings.HasPrefix(title, ticket) {
		return title
	}
	desc := title
	if m := titleRe.FindStringSubmatch(title); len(m) > 0 && validTypes[m[1]] {
		desc = strings.TrimSpace(m[4])
	}
	if desc == "" {
		return ticket
	}
	return ticket + ": " + desc
}

func TightenTitle(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}

	m := titleRe.FindStringSubmatch(title)
	if len(m) == 0 || !validTypes[m[1]] {
		return inferType(title) + ": " + title
	}
	return title
}

func inferType(text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))
	switch {
	case hasDocumentationLanguage(lower):
		return "docs"
	case hasProductImpactLanguage(lower) || isFeatureLanguage(lower) || isFixLanguage(lower):
		return inferReleaseType(lower)
	default:
		return "chore"
	}
}

func inferReleaseType(text string) string {
	if isFeatureLanguage(text) {
		return "feat"
	}
	return "fix"
}

func isFixLanguage(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	fixPrefixes := []string{
		"fix ", "fixes ", "fixed ", "resolve ", "resolves ", "resolved ",
		"correct ", "corrects ", "corrected ", "repair ", "repairs ", "repaired ",
	}
	for _, prefix := range fixPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func isFeatureLanguage(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	featurePrefixes := []string{
		"add ", "adds ", "added ", "introduce ", "introduces ", "introduced ",
		"create ", "creates ", "created ", "implement ", "implements ", "implemented ",
		"support ", "supports ", "supported ", "enable ", "enables ", "enabled ",
		"allow ", "allows ", "allowed ",
	}
	for _, prefix := range featurePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return strings.Contains(lower, " new ") || strings.HasPrefix(lower, "new ")
}

func hasProductImpactLanguage(text string) bool {
	lower := strings.ToLower(text)
	terms := []string{
		"user-facing", "user visible", "user-visible", "user experience", " ux", "ux ",
		" ui", "ui ", "cli", "command", "output", "behavior", "workflow",
		"prompt", "flag", "error message",
	}
	for _, term := range terms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func hasDocumentationLanguage(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "readme") || strings.Contains(lower, "documentation") || strings.Contains(lower, "docs")
}
