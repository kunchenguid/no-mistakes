package agent

func ExecutionContextPromptSection(workDir string) string {
	return `
Execution context:
- You are running inside an isolated git worktree at the current working directory, whose exact absolute path is: ` + workDir + `
- Path contract: every project source file you read or edit lives under that exact path. Prefer relative paths; when a tool requires an absolute path, use exactly ` + workDir + ` as the prefix - never invent, guess, abbreviate, or re-resolve paths (no /private/tmp substitutions, no other parents). If you are unsure where a file is, list the directory first instead of guessing its location. The backing Git metadata allowance below is an exception for metadata reads, not another source checkout.
- The worktree's ` + "`.git`" + ` is a pointer file (not a directory) referencing a bare gate repository elsewhere on disk; this is standard git-worktree layout and all normal git commands work as expected.
- The worktree is checked out to the change being processed; treat it as the project's source of truth for this run and do not search the filesystem for "the real" checkout - this is it.
- Operate on project source only within this working directory. Use Git through this worktree for operations appropriate to the assigned phase; do not directly modify the gate's bare repository or operate on any other clone of this project.
- An isolated read-only child may have selective read-only access to this worktree's authentic backing Git metadata needed for those operations, at its original absolute paths. Preserve the source identity, original ` + "`.git`" + ` pointer, Git/common directories, index/history, and inherited gate markers. Do not replace the pointer or substitute another repository. The child must not write source or backing Git metadata; this allowance does not expose the entire bare gate repository.
- Do not expose operational databases/configuration, control sockets, credentials, trust stores, unrelated repositories, or foreign endpoints to that child. If authentic Git identity and these boundaries cannot both be preserved, report the blocker rather than widening access or fabricating metadata.
`
}
