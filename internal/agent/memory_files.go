package agent

// MemoryFilesRule is the hands-off instruction appended to every pipeline
// prompt whose agent can write to the worktree. AGENTS.md and CLAUDE.md are
// loaded into every future agent session, so what they contain is a
// deliberate human decision about what every session pays tokens for -
// additions and rewrites are never automated pipeline output. The document
// step is the single exception: it owns a narrower correction-only wording
// (internal/pipeline/steps/document.go) instead of this rule, and the
// rebase/merge conflict resolvers use MemoryFilesConflictRule because a
// conflicted memory file still has to be resolved for the integration to
// conclude. This is a prompt contract, not a sandbox: agents keep free file
// access, so the behavioral tests in internal/pipeline/steps pin the wording
// on the emitted prompts.
const MemoryFilesRule = `

Agent memory files (AGENTS.md and CLAUDE.md) are hands-off:
- Do not create, modify, rename, or delete them - not even to correct or add content that looks stale, wrong, or missing.
- They load into every future agent session, so what they contain stays a deliberate human decision, never automated pipeline output.`

// MemoryFilesConflictRule scopes the hands-off rule for the rebase and merge
// conflict-resolution prompts. When a memory file is itself conflicted it
// must still be resolved like any other file or the integration cannot
// conclude, so the carve-out allows resolving the conflict itself, including
// marker-less conflicts, but no unrelated content edits.
const MemoryFilesConflictRule = `- Agent memory files AGENTS.md and CLAUDE.md stay hands-off beyond the conflict itself: resolve their conflicts yourself whether they have conflict markers or are modify/delete or add/add conflicts. For modify/delete, decide whether to keep or remove the file based on the two sides; stage the resolution. Make no other edits to their content - never add, rewrite, or restructure it outside the conflict.`
