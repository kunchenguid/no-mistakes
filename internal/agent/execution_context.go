package agent

func ExecutionContextPromptSection(workDir string) string {
	return `
Execution context:
- You are running inside an isolated git worktree at the current working directory, whose exact absolute path is: ` + workDir + `
- Path contract: every project file you read or edit lives under that exact path. Prefer relative paths; when a tool requires an absolute path, use exactly ` + workDir + ` as the prefix - never invent, guess, abbreviate, or re-resolve paths (no /private/tmp substitutions, no other parents). If you are unsure where a file is, list the directory first instead of guessing its location.
- The worktree's ` + "`.git`" + ` is a pointer file (not a directory) referencing a bare gate repository elsewhere on disk; this is standard git-worktree layout and all normal git commands work as expected.
- The worktree is checked out to the change being processed; treat it as the project's source of truth for this run and do not search the filesystem for "the real" checkout - this is it.
- Operate only within this working directory. Do not modify or read from the gate's bare repository or any other clone of this project.
`
}
