package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

const gitIdentityHelp = "Configure your own identity in this repository with `git config user.name \"Your Name\"` and `git config user.email \"you@example.com\"` (or use --global for your default identity). Check author/committer config and GIT_AUTHOR_*/GIT_COMMITTER_* overrides if those settings are already present; do not disable user.useConfigOnly."

// checkGitCommitIdentity asks Git to resolve both roles in the caller's current
// repository/environment. Reading user.name/email alone would miss includes,
// role-specific config, environment overrides, and user.useConfigOnly. This is
// a read-only preflight, not an identity pin for the daemon's later commits.
func checkGitCommitIdentity(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, role := range []string{"AUTHOR", "COMMITTER"} {
		variable := "GIT_" + role + "_IDENT"
		cmd := exec.CommandContext(ctx, "git", "var", variable)
		winproc.Harden(cmd)
		out, err := cmd.Output()
		if err != nil {
			// Do not echo Git's stderr: it can include the resolved identity.
			return fmt.Errorf("cannot resolve Git %s identity (%s): %w", strings.ToLower(role), variable, err)
		}
		// Git rejects an empty name, but accepts an explicitly empty email
		// ("Name <> ..."). Check the resolved fields, not just its exit code.
		name, rest, ok := strings.Cut(string(out), "<")
		email, _, closed := strings.Cut(rest, ">")
		if !ok || !closed || strings.TrimSpace(name) == "" || strings.TrimSpace(email) == "" {
			return fmt.Errorf("Git %s identity has an empty name or email (%s)", strings.ToLower(role), variable)
		}
	}
	return nil
}
