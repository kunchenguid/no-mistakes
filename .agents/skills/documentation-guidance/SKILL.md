---
name: documentation-guidance
description: Use when changing documentation ownership, generated agent guidance, review auto-fix guidance, the combined document+lint housekeeping pass, or the agent memory-file prompt rules.
user-invocable: false
metadata:
  internal: true
---

**Documentation**

- Keep `README.md` concise and high-level; the bar needs to be extremely high for what shows up there.
- Most documentation lives in `docs/`, the published docs site.
- One owner per fact: `docs/src/content/docs/reference/global-config.md` and `docs/src/content/docs/reference/repo-config.md` own configuration keys, `docs/src/content/docs/reference/environment.md` owns environment variables and the telemetry local/remote split, `docs/src/content/docs/concepts/daemon.md` owns the daemon lifecycle model, and guides pages explain purpose and link to those owners instead of restating tables and examples.
- The `document.instructions` block in `.no-mistakes.yaml` states this ownership map for the pipeline's document step; update it when ownership moves.

**Agent-Guidance Surfaces**

- `skills/no-mistakes/SKILL.md` is **generated**: the source of truth is the `body` constant in `internal/skill/skill.go`. Edit the body, then `make skill`; `make lint` fails CI on drift. Never edit `SKILL.md` directly. `no-mistakes init` ships this rendering to agents at user level.
- Agent-driving guidance is owned by the skill body and the live `axi` output strings (`internal/cli/axi*.go`); `docs/src/content/docs/guides/agents.md` carries only the canonical invariant sentences pinned by `internal/cli/axi_guidance_test.go` plus a pointer to the skill. When you change driving guidance, change the skill body and the point-of-use `axi` strings together; that drift test is the sync check.
- The shared default test-quality rule lives in `internal/testguidance`; render it only into the task-first skill and pipeline roles that can author, repair, or review tests. Its fake-agent prompt tests are the intentional generated-interface contract, not source-text checks.
- Review auto-fix is disabled by default (`auto_fix.review: 0` in `config.go` `autoFixDefaults`), so blocking and ask-user review findings park for an agent decision; keep the skill, the live `axi` gate `note`, and docs qualified if you touch review auto-fix.

**Combined Document+Lint Housekeeping Pass**

- When `commands.lint` is empty, the document step performs both duties in one agent invocation and stashes the lint half on `RunShared` (consume-once); the lint step consumes it instead of paying a second cold pass. Neither duty is ever silently dropped: a skipped pass, untrusted structured output, or a lint fix round falls back to lint's own agent pass. Configured `commands.lint` stays a first-class deterministic gate. Uncategorized findings fail safe to the stricter documentation gate.
- The document prompt enforces the placement policy (one owner per fact, stale duplicates become pointers, no AGENTS.md postmortems, scope limited to docs the change made stale). Do not reintroduce exhaustive-corpus-sweep language; it caused doc commits in 90 of 121 audited PRs. Contract test: `TestDocumentStep_PromptAppliesPlacementPolicy`; behavior tests: `internal/pipeline/steps/housekeeping_test.go`.
- `agent.MemoryFilesRule` (`internal/agent/memory_files.go`) limits independently initiated memory-file edits without excluding memory-file changes from review or prompted fixes. It is appended to the review/test/lint/PR/intent prompts and through `fixerPrompt` to shared fix turns and CI repair; rebase/merge conflict resolvers use `agent.MemoryFilesConflictRule`, and the document prompt has its own correction-only rule. See `docs/src/content/docs/reference/pipeline-steps.md` for the user-facing contract. Prompt-contract tests: `internal/pipeline/steps/memory_files_test.go`, `internal/intent/*_test.go`.
