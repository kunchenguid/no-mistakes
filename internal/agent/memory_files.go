package agent

// MemoryFilesRule limits edits a pipeline agent initiates on its own. A memory
// file that is part of the change under validation is still reviewed like any
// other file, and a fix agent may address a finding or recorded human fix
// decision about that change. The document step owns a narrower
// correction-only rule (internal/pipeline/steps/document.go); the rebase/merge
// conflict resolvers use MemoryFilesConflictRule to resolve conflicts without
// unrelated edits. This is a prompt contract, not a sandbox: agents keep free
// file access, so the behavioral tests in internal/pipeline/steps pin the
// wording on the emitted prompts.
const MemoryFilesRule = `

Agent memory files (AGENTS.md and CLAUDE.md) - limits on your own changes:
- This rule governs edits you would initiate on your own as pipeline work. Do not independently create, modify, rename, or delete these files, and do not add or rewrite their content just because something seems missing, stale, or wrong.
- If these files are part of the change under validation, review their changes like any other file. Never flag them merely for being changed; you may report inaccurate content as you would in any other file.
- In a fix turn, when a finding or recorded human fix decision concerns memory-file content that is part of the change under validation, you may edit those files to address it. Do not make unrelated or otherwise unprompted memory-file edits.
- They load into every future agent session, so additions or rewrites outside those prompted changes remain a deliberate human decision, not automated pipeline output.`

// MemoryFilesConflictRule scopes the independent-edit limit for rebase and
// merge conflict-resolution prompts. A conflicted memory file must still be
// resolved for the integration to conclude, so the carve-out allows resolving
// the conflict itself, including marker-less conflicts, but no unrelated
// content edits.
const MemoryFilesConflictRule = `- For AGENTS.md and CLAUDE.md, make no independent content edits beyond resolving the conflict itself: resolve their conflicts yourself whether they have conflict markers or are modify/delete or add/add conflicts. For modify/delete, decide whether to keep or remove the file based on the two sides; stage the resolution. Make no other edits to their content - never add, rewrite, or restructure it outside the conflict.`
