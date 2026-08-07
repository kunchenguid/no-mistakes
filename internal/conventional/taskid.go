package conventional

import (
	"fmt"
	"strings"
)

// TaskIDFormat selects where a task-tracking id is placed in a PR title.
type TaskIDFormat string

const (
	// TaskIDFormatReleasePlease appends " (ID)" to the description and is the
	// release-safe default: "type(scope):" stays at position 0, so
	// release-please and any other conventional-commit parser still detect the
	// type and the changelog is unaffected.
	TaskIDFormatReleasePlease TaskIDFormat = "release-please"
	// TaskIDFormatPrefix puts "[ID] " in front of the conventional title. It
	// reads best in a PR list, but the title is no longer conventional, which a
	// strict release-please setup will reject. That is the caller's opt-in
	// tradeoff.
	TaskIDFormatPrefix TaskIDFormat = "prefix"
	// TaskIDFormatSuffix appends " [ID]" after the conventional title.
	TaskIDFormatSuffix TaskIDFormat = "suffix"
)

// DefaultTaskIDFormat is used whenever no format was chosen, including for run
// rows written before the format was persisted. It is the release-safe one on
// purpose: an unset format must never silently break release automation.
const DefaultTaskIDFormat = TaskIDFormatReleasePlease

// maxTaskIDLength bounds a tracking id so it cannot crowd out the description
// in a provider title field. Real ids (Jira keys, issue numbers, Asana/ClickUp
// ids) are far shorter than this.
const maxTaskIDLength = 64

// TaskIDFormats returns every supported format, in the order they are offered
// to users.
func TaskIDFormats() []TaskIDFormat {
	return []TaskIDFormat{TaskIDFormatReleasePlease, TaskIDFormatPrefix, TaskIDFormatSuffix}
}

// ParseTaskIDFormat validates a user-supplied format name. An empty value
// resolves to DefaultTaskIDFormat.
func ParseTaskIDFormat(value string) (TaskIDFormat, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return DefaultTaskIDFormat, nil
	}
	for _, format := range TaskIDFormats() {
		if TaskIDFormat(trimmed) == format {
			return format, nil
		}
	}
	names := make([]string, 0, len(TaskIDFormats()))
	for _, format := range TaskIDFormats() {
		names = append(names, string(format))
	}
	return "", fmt.Errorf("unknown task id format %q: valid formats are %s", trimmed, strings.Join(names, ", "))
}

// ValidateTaskID rejects ids that cannot safely become part of a PR title.
// A title is a single line, so control characters are refused outright rather
// than silently mangled by the provider.
func ValidateTaskID(id string) error {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return fmt.Errorf("task id is empty")
	}
	if len(trimmed) > maxTaskIDLength {
		return fmt.Errorf("task id is %d characters, longer than the %d-character limit", len(trimmed), maxTaskIDLength)
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("task id contains a control character")
		}
	}
	return nil
}

// TaskID pairs a provider-neutral tracking id with the way it renders into a
// PR title. The zero value carries no id and every operation on it is a no-op.
type TaskID struct {
	ID     string
	Format TaskIDFormat
}

// Empty reports whether there is no usable tracking id to apply.
func (t TaskID) Empty() bool {
	return ValidateTaskID(t.ID) != nil
}

// EffectiveFormat is the single owner of the "unset or unknown format means
// DefaultTaskIDFormat" rule: Apply places the id by it, and callers that report
// the placement read it so their message always matches the rendered title.
func (t TaskID) EffectiveFormat() TaskIDFormat {
	for _, format := range TaskIDFormats() {
		if t.Format == format {
			return format
		}
	}
	return DefaultTaskIDFormat
}

// Apply bakes the tracking id into an already-final PR title.
//
// It must run AFTER TightenTitle, never before: feeding a decorated title such
// as "[WA-3093] fix(x): y" through TightenTitle yields "chore: [WA-3093] fix(x):
// y", which destroys the type release automation parses.
//
// Apply is idempotent. A title that already carries the id anywhere is returned
// untouched, so re-runs against an existing PR never accrete copies of it.
func (t TaskID) Apply(title string) string {
	title = strings.TrimSpace(title)
	id := strings.TrimSpace(t.ID)
	if title == "" || t.Empty() {
		return title
	}
	if taskIDPresent(title, id) {
		return title
	}
	switch t.EffectiveFormat() {
	case TaskIDFormatPrefix:
		return "[" + id + "] " + title
	case TaskIDFormatSuffix:
		return title + " [" + id + "]"
	default:
		return title + " (" + id + ")"
	}
}

// taskIDPresent reports whether title already contains id as a whole token, so
// "WA-309" is not considered present in a title that only mentions "WA-3093".
func taskIDPresent(title, id string) bool {
	for i := 0; i+len(id) <= len(title); i++ {
		if title[i:i+len(id)] != id {
			continue
		}
		if i > 0 && isTaskIDTokenChar(title[i-1]) {
			continue
		}
		if end := i + len(id); end < len(title) && isTaskIDTokenChar(title[end]) {
			continue
		}
		return true
	}
	return false
}

// isTaskIDTokenChar reports whether c would extend an adjacent match into a
// longer identifier (so a match on it is not a whole-token match).
func isTaskIDTokenChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	}
	return c == '_' || c == '-'
}
