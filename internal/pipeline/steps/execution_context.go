package steps

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
func executionContextPromptSection() string {
	return `
Execution context:
- You are running inside an isolated git worktree at the current working directory.
- The worktree's ` + "`.git`" + ` is a pointer file (not a directory) referencing a bare gate repository elsewhere on disk; this is standard git-worktree layout and all normal git commands work as expected.
- The worktree is checked out to the change being processed; treat it as the project's source of truth for this run and do not search the filesystem for "the real" checkout - this is it.
- Operate only within this working directory. Do not modify or read from the gate's bare repository or any other clone of this project.
`
}

// toolBatchingPromptSection encourages agents that expose Code Mode or an exec
// cell to batch independent read-only tool calls without reordering dependent
// or mutating work.
func toolBatchingPromptSection() string {
	return `
Bounded tool-call stages:
- When your environment supports Code Mode/an exec cell, if you have two or more independent read-only shell/tool calls, run them concurrently in one exec cell using ` + "`Promise.allSettled`" + ` or ` + "`Promise.all`" + `, inspect every result, and do not split that batch across separate outer tool calls.
- Keep dependent, ordering-sensitive, and mutating work sequential. Do not parallelize commands whose outputs determine later commands.
`
}
