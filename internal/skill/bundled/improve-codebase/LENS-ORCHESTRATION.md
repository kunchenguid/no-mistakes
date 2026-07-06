# Lens Orchestration

Self-contained orchestration guide for `improve-codebase`. This package has no
required dependency on any other skill, extension, external tool, hosted
service, documentation source, or runtime-specific team mode.

## Core invariant

| Rule | Meaning |
| --- | --- |
| Five lens reports stay mandatory | Module Topology and File-Size Balance, Deepening, Operational Mechanics, Code Health and Cleanup, and Change-Set Quality each return a standalone report. |
| Local evidence only is enough | The audit can complete from repository files, tests, configs, local git state, and locally discoverable commands. |
| Read-only by default | Source, docs, config, tests, external systems, and repo history are not mutated during the audit. Audit artifacts in the OS temp directory are allowed. |
| Parent synthesizes | The parent compares lens outputs, records contradictions, and maps findings through `FINDING-REGISTRY.md`. |

## Execution modes

Use the strongest execution mode available in the current runtime:

1. **Parallel specialist passes**: run five independent read-only lens passes in parallel when subagents or equivalent isolated workers are available.
2. **Sequential isolated passes**: if parallel workers are unavailable, run the five lenses one at a time with fresh lens-specific notes and no synthesis until all five are complete.
3. **Blocked or partial**: if a lens cannot inspect required local evidence, mark that lens `blocked` or `partial-low-confidence`; do not invent its findings from another lens.

Subagents are an optimization, not a dependency. A missing subagent feature must
not block the skill if sequential isolated passes can still produce five
standalone reports.

## Lens briefs

Every lens brief gets the same shared setup:

- repo root and requested scope;
- whether scope is whole-codebase or explicitly narrowed;
- relevant source roots, tests, package/build files, docs, configs, and entrypoints;
- local change horizon from git when available;
- discovered validation commands;
- artifact paths planned for Markdown and HTML reports;
- known context/runtime limits.

Then give each lens only its own companion docs:

| Lens | Required docs | Owns |
| --- | --- | --- |
| Module Topology and File-Size Balance | `LANGUAGE.md`, `MODULE-TOPOLOGY.md` | File size, hierarchy, workspace layout, related-file clusters, package homes, stranded facades/siblings. |
| Deepening | `LANGUAGE.md`, `DEEPENING.md` | Shallow modules, weak interfaces, seams, locality, leverage, test surfaces, speculative abstractions. |
| Operational Mechanics | `LANGUAGE.md`, `OPERATIONAL-MECHANICS.md` | Repeated provider/SDK/workflow/infrastructure mechanics and shared-mechanics module candidates. |
| Code Health and Cleanup | `LANGUAGE.md`, `CODE-HEALTH.md` | Decay risks, debt, tests, health scoring, cleanup safety, prove-or-delete ledger. |
| Change-Set Quality | `LANGUAGE.md`, `CHANGE-SET-QUALITY.md` | Local/recent work quality, touched-area regressions, missed simplification, fallback high-risk areas. |

## Statuses

| Status | Use when | Required handling |
| --- | --- | --- |
| `complete` | The lens inspected enough local evidence and returned a standalone report. | Cite evidence and affected findings. |
| `blocked` | Required local evidence or readable files are unavailable. | Name blocker, risk, and next validation step. |
| `partial-low-confidence` | The lens ran with truncated scope, weak evidence, missing validation, or context limits. | Name what is missing and lower confidence on affected findings. |
| `not-applicable` | A lens sub-section has no evidence in this repo, such as no git history for change-set comparison. | State the reason; keep the lens report present. |

## Closure rules

- All five lens reports must be present for a completed audit.
- A missing mandatory lens makes the whole audit `blocked` or `partial-low-confidence`.
- Unresolved contradictions must appear in the Relationship Map and either lower confidence or become a contradiction finding.
- Synthesis may prioritize findings for navigation, but may not delete or hide lens-level detail.
- Optional local evidence sources, such as git upstream, generated dependency graphs, or test coverage output, improve confidence only when available. Their absence is not an external dependency failure.

## Side-effect boundaries

| Surface | Default | Rule |
| --- | --- | --- |
| Repo files | Read-only. | Do not edit source, tests, docs, configs, generated code, or lockfiles. |
| Git | Read-only. | Inspect status, diffs, logs, and merge-base when available; do not commit, reset, checkout, stash, push, tag, or rebase. |
| External systems | Out of scope. | Do not rely on hosted services, cloud consoles, workflow tools, or account state. |
| Audit artifacts | Allowed. | Write Markdown and offline HTML reports to the OS temp directory only. |

## Forward-test constraints

Use these constraints when validating this skill package:

- Test with a realistic fresh prompt against a generic repo.
- Do not leak expected findings, expected wording, or hidden fixes.
- Accept only when the transcript or artifact shows five lens reports, local-only evidence, read-only behavior, standalone links, and honest status reporting.
