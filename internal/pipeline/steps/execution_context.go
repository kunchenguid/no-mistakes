package steps

import "github.com/kunchenguid/no-mistakes/internal/agent"

// executionContextPromptSection returns a prompt fragment that explains the
// agent's runtime environment: it is operating inside an isolated git
// worktree carved from a bare gate repository, not in the original repo.
//
// Why this exists: agents that scan their cwd to "verify" the project
// (Claude Code, opencode, etc.) frequently misread a worktree's .git
// pointer-file as "not a git repository" and either bail out or go
// hunting for the real checkout, sometimes ending up at the bare gate
// repo. The fix is not to lie about the cwd - it's to spell out what
// the cwd actually is so the agent can stop second-guessing it.
//
// The fragment ends with a trailing newline so callers can append it
// directly to a prompt string without worrying about spacing.
func executionContextPromptSection(workDir string) string {
	return agent.ExecutionContextPromptSection(workDir)
}
