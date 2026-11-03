package safepath

import (
	"path/filepath"
	"strings"
	"testing"
)

// A JSON string escape right after a repo-level worktrees path is the escape,
// not a separator plus a run id, so the next word survives.
func TestRedactWorktrees_KeepsJSONEscapeAfterRepoLevelPath(t *testing.T) {
	worktrees := filepath.Join(t.TempDir(), "worktrees")
	for _, esc := range []string{`\n`, `\r`, `\t`} {
		// A slash-spelled root: a backslash after it can only start an escape.
		text := filepath.ToSlash(filepath.Join(worktrees, "ab12")) + esc + "next"
		if got, want := RedactWorktrees(text, "", worktrees), WorktreesPlaceholder+esc+"next"; got != want {
			t.Errorf("RedactWorktrees(%q) = %q, want %q", text, got, want)
		}
	}
}

// In text that spells the root with single backslashes (native Windows
// output), a backslash before a run id that starts with n, r or t is still a
// separator.
func TestRedactWorktrees_SingleBackslashRootKeepsBackslashSeparators(t *testing.T) {
	worktrees := filepath.Join(t.TempDir(), "worktrees")
	native := strings.ReplaceAll(worktrees, "/", `\`)
	text := native + `\ab12\run-0\out.log`
	if got, want := RedactWorktrees(text, "", worktrees), WorktreePlaceholder+`\out.log`; got != want {
		t.Errorf("RedactWorktrees(%q) = %q, want %q", text, got, want)
	}
}
