---
name: improve-codebase
description: "Explicit-only read-only whole-codebase improvement audit across module topology, file-size balance, seams, repeated operational mechanics, code health, cleanup safety, test quality, and local change-set quality. Use only when the user explicitly invokes improve-codebase or asks to run the full codebase improvement audit. Narrow only when the user names a path/scope. Standalone package with no required external tooling or hosted services."
---

# Improve Codebase

Run one read-only, project-agnostic audit through five built-in lenses:

- **Module Topology and File-Size Balance**: module topology, file/code-line size, hierarchy, workspace folder structure, related-file clusters, flat structure, and package/home recommendations.
- **Deepening**: shallow modules, weak seams, over-engineered abstractions, low locality/leverage, poor interfaces, and poor test surfaces.
- **Operational Mechanics**: repeated provider/SDK/workflow/infrastructure mechanics that may deserve shared-mechanics modules.
- **Code Health and Cleanup**: code decay, architecture health, tech debt, test quality, health scoring, cleanup safety, and the strict prove-or-delete ledger for legacy/dead/unnecessary/compatibility-smell code and tests.
- **Change-Set Quality Gate**: local committed/uncommitted work, touched-area structure, and high-risk areas that may have gotten worse.

This skill is audit-only by default. Do not edit source, tests, docs, configs,
tracker items, or external systems. Temporary Markdown and HTML audit artifacts
in the OS temp directory are allowed outputs.

## no-mistakes pipeline gate mode

When explicitly invoked by the no-mistakes pipeline as a read-only
structural/change-set gate with a JSON schema:

- Treat the no-mistakes prompt as the scope boundary.
- Use the package vocabulary and lens guidance that fit the bounded change-set.
- Do not write Markdown or HTML artifacts.
- Do not open an HTML report.
- Do not run tests, formatters, linters, commits, pushes, or grilling loops.
- Return only the schema-constrained JSON findings requested by the pipeline.
- Keep normal standalone audit artifact requirements for every non-pipeline use.

## Scope rules

- Default scope is the whole codebase/project.
- Narrow scope only when the user clearly names a path, package, service, workflow, bounded area, branch, PR, diff, or change range.
- When narrowed, include callers, tests, configs, and relevant docs needed to understand impact.
- Report only `Scope`; do not invent named audit labels.
- Do not silently narrow to the current diff, recent files, largest files, or a single problem area.
- Local changes, touched areas, and high-risk areas are evidence streams within the scoped audit unless the user explicitly made them the boundary.
- Recommendation scope is broad even though execution is read-only. Large refactors, structural moves, logic/system changes, module migrations, compatibility deprecations, and test-topology changes are fair recommendations when they improve the codebase and have a credible validation path.
- Almost nothing in codebase improvement is out of scope for recommendation unless it violates explicit user/repo safety boundaries, secrets/data handling rules, live/API restrictions, or a human product/security/business policy judgment that cannot be inferred from the codebase.

## Read first

Load only the references needed for the current scope:

- [LANGUAGE.md](LANGUAGE.md) — vocabulary for the audit.
- [LENS-ORCHESTRATION.md](LENS-ORCHESTRATION.md) — standalone lens execution, fallback, statuses, and side-effect boundaries.
- [MODULE-TOPOLOGY.md](MODULE-TOPOLOGY.md) — structural inventory for topology, file size, hierarchy, workspace folders, related-file clusters, and package homes.
- [DEEPENING.md](DEEPENING.md) — dependency categories, seam discipline, and test strategy for deepening candidates.
- [OPERATIONAL-MECHANICS.md](OPERATIONAL-MECHANICS.md) — repeated operational logic and shared-mechanics module candidates.
- [CODE-HEALTH.md](CODE-HEALTH.md) — decay, debt, test, health, cleanup safety, and prove-or-delete ledger.
- [CHANGE-SET-QUALITY.md](CHANGE-SET-QUALITY.md) — strict review of local work, touched areas, and fallback high-risk areas.
- [FINDING-REGISTRY.md](FINDING-REGISTRY.md) — complete finding ledger, evidence grades, relationships, statuses, and remediation paths.
- [HTML-REPORT.md](HTML-REPORT.md) — offline visual report guidance.

Source-of-truth ownership: this `SKILL.md` owns trigger, scope,
orchestration, non-negotiables, output order, artifact requirements, and final
report shape. Companion docs own their named lens details and report schemas.

## Orchestration

- Complete exactly five independent read-only lens passes.
- Prefer parallel specialist passes when the runtime supports them.
- Fall back to sequential isolated lens passes when parallel workers are unavailable.
- Do not call an audit complete if a mandatory lens is missing.
- Do not block merely because external tooling or hosted services are unavailable.
- One parent/orchestrator synthesizes all lens output, records contradictions, and reports the result.
- Lens prompts must be self-contained. Do not create nested teams.
- Missing local evidence may make a lens `blocked` or `partial-low-confidence`; it does not authorize inventing findings from another lens.

## Audit preflight

Before launching lens work, record a compact preflight block in the audit notes:

- Repo root, requested scope, and whether scope is whole-codebase or explicitly narrowed.
- Execution mode: parallel specialist passes, sequential isolated passes, blocked, or partial-low-confidence.
- Companion docs available for this package and any missing local package reference.
- Local evidence sources available: source roots, tests, docs, configs, package/build files, local git state, and validation commands.
- Local change horizon: staged changes, unstaged changes, local commits, upstream/range fallback, and confidence impact.
- Discovered verification commands for tests, typechecks, builds, lint, docs, or "needs validation discovery".
- Planned Markdown and HTML artifact paths.
- Context/runtime limits that may force a Not Reviewed / Continuation Ledger.

If package companion docs are missing, stop as blocked. If local repo evidence is
missing or truncated, continue only as partial-low-confidence with the limits named.

## Process

1. **Scope**
   - Identify repo root, source roots, tests, docs, configs, package/build files, and relevant entrypoints.
   - Establish the audit scope. Default to the whole codebase/project; narrow only for an explicit user-named boundary.
   - Build a local change horizon from git when available: uncommitted changes, staged changes, local commits not on the upstream branch, recently touched files, and any user-supplied range.
   - Use local changes to inform Change-Set Quality; do not let them replace whole-codebase scope unless the user explicitly requested that boundary.
   - Assign all topology, file-size, hierarchy, workspace folder, related-file cluster, and package/home work to Module Topology and File-Size Balance.

2. **Run five lens passes**
   - **Module Topology and File-Size Balance**: read [LANGUAGE.md](LANGUAGE.md) and [MODULE-TOPOLOGY.md](MODULE-TOPOLOGY.md); perform the complete structural inventory.
   - **Deepening**: read [LANGUAGE.md](LANGUAGE.md) and [DEEPENING.md](DEEPENING.md); map shallow modules, weak interfaces, leaky seams, over-engineered shallow abstractions, speculative seams/options/adapters, low locality/leverage, poor test surfaces, and agent-navigation friction.
   - **Operational Mechanics**: read [LANGUAGE.md](LANGUAGE.md) and [OPERATIONAL-MECHANICS.md](OPERATIONAL-MECHANICS.md); map every meaningful repeated provider/SDK/workflow/infrastructure mechanic and its callers.
   - **Code Health and Cleanup**: read [LANGUAGE.md](LANGUAGE.md) and [CODE-HEALTH.md](CODE-HEALTH.md); run local/recent code decay, architecture health, tech debt, test quality, health scoring, cleanup safety, and the prove-or-delete ledger.
   - **Change-Set Quality**: read [CHANGE-SET-QUALITY.md](CHANGE-SET-QUALITY.md); review local committed/uncommitted work, touched modules/callers/tests, and fallback high-risk areas for structural regression, avoidable complexity, spaghetti branching, weak boundaries, and missed simplification.
   - The parent coordinates scope, waits for all lens reports, checks contradictions, then packages five full lens reports plus the complete registry, relationship map, static facet index, remediation paths, and deferred/watchlist ledger.

3. **Synthesize**
   - Preserve five full standalone lens reports. They are first-class outputs, not intermediate notes.
   - Read [FINDING-REGISTRY.md](FINDING-REGISTRY.md) before assigning evidence grades, cleanup dispositions, relationships, or statuses.
   - Add every lens finding to the complete Finding Registry.
   - Build a Cross-Lens Relationship Map for duplicates, overlaps, supports, contradictions, same-root-cause clusters, and same-remediation clusters.
   - Build a static Facet Index for files/modules, severity, confidence, evidence grade, safety, validation command, and root-cause/remediation cluster.
   - Reconcile contradictions explicitly. Do not silently choose one lens over another without evidence and cited lens IDs.
   - Do not collapse distinct findings into a generalized candidate. If findings overlap, cross-reference them by lens finding ID.
   - Include all material findings from each lens, including lower-priority or conflicting findings, as long as they are evidence-backed and relevant to the requested scope.
   - Add a separate cross-lens synthesis after the registry and facets. Synthesis is navigation, not suppression.
   - Every synthesized recommendation must trace back to one or more lens finding IDs and include depth/locality, operational duplication when relevant, health risk, safety class, and validation path.
   - Include the Code Health prove-or-delete ledger in full.
   - Treat unproven legacy, dead, unnecessary, compatibility-smell, or over-engineered code/tests as actionable findings.
   - Prefer `active` or `safe-cleanup` when an evidence-backed improvement can be made now with characterization tests and local validators.
   - Use `deferred-watchlist` only for genuinely non-actionable-yet issues with concrete evidence and a concrete revisit trigger.
   - Use `needs-domain-decision` only when the remedy requires a human product, security, business, ownership, or domain policy decision that cannot be inferred from code.

4. **Final validation**
   - Confirm all five standalone lens reports are present, or mark the audit blocked/partial-low-confidence with missing lenses named.
   - Confirm every Finding Registry row has the required fields from [FINDING-REGISTRY.md](FINDING-REGISTRY.md).
   - Confirm every synthesis recommendation cites lens finding IDs and every relationship cites target IDs or says `none observed`.
   - Confirm every deferred/watchlist row has concrete evidence, a genuine blocker, and a revisit trigger.
   - Confirm Markdown and HTML artifacts were written, or state why an artifact could not be written.
   - Confirm HTML opening was attempted or explicitly skipped because the environment is headless, remote, or restricted.
   - Confirm the Not Reviewed / Continuation Ledger is present when scope, runtime, context, or missing local evidence prevented completion.

5. **Report**
   - Complete artifacts are required for every completed audit.
   - Return the complete Markdown audit in chat when it fits. If a single response cannot hold it, write the full artifact and make the chat output a faithful navigable index instead of a lossy summary.
   - Also write the complete Markdown audit to the OS temp directory: `<tmpdir>/improve-codebase-audit-<timestamp>.md`.
   - Also write a self-contained offline HTML report to the OS temp directory: `<tmpdir>/improve-codebase-audit-<timestamp>.html`.
   - Open the HTML report when feasible; in headless, remote, or restricted environments, skip opening it and say so. Give both absolute paths.
   - If scope, runtime, context, or missing local evidence prevents completion, include a Not Reviewed / Continuation Ledger with omitted scope, reason, risk, and next validation step.
   - End with an implementation-ready remediation backlog, not a question.

## Markdown output

Use this shape:

```md
# Improve Codebase Audit

**Scope:** <whole codebase/project, or explicit path/package/service/change range requested by user>
**Confidence:** <high|medium|low and why>
**Lens execution:** <five independent lens passes completed, partial-low-confidence with missing lenses named, or blocked>
**Markdown report:** <absolute temp path or "not written">
**HTML report:** <absolute temp path or "not written">

## Audit Preflight

- Repo root:
- Scope mode:
- Execution mode:
- Five-lens feasibility:
- Companion docs available/missing:
- Local evidence sources:
- Local change horizon:
- Verification commands discovered:
- Planned artifact paths:
- Context/runtime limits:

## Lens 1: Module Topology and File-Size Balance

### Oversized split suspects (>1000 code lines)
| File | Code lines | Current responsibility mix | Proposed module/submodule split | Evidence | Status |
| --- | ---: | --- | --- | --- | --- |

### Undersized merge suspects (<200 code lines)
| Files/cluster | Code lines | Shared responsibility | Proposed merge/home module | Evidence | Status |
| --- | ---: | --- | --- | --- | --- |

### Related-file directory clusters (3+ same-module siblings)
| Directory | Files/cluster | Shared module/submodule | Proposed directory/package home | Evidence | Status |
| --- | --- | --- | --- | --- | --- |

### Stranded facade / stranded sibling suspects
| Parent directory | Dedicated package | Left-behind sibling/facade | Required disposition | Evidence | Status |
| --- | --- | --- | --- | --- | --- |

### T1. <structural finding>
- Files/directories:
- Structural category:
- Code-line evidence:
- Hierarchy evidence:
- Proposed module/submodule home:
- Package/interface recommendation:
- Compatibility/deprecation disposition:
- Confidence:
- Evidence grade:
- Safety:
- Relationship notes:
- Validation:

## Lens 2: Deepening

### D1. <finding>
- Files:
- Evidence:
- Recommendation:
- Dependency category:
- Confidence:
- Evidence grade:
- Safety:
- Relationship notes:
- Test surface:
- Validation:

## Lens 3: Operational Mechanics

### O1. <finding>
- Files:
- Repeated mechanic:
- Current callers:
- Proposed shared operational interface:
- Caller-owned policy/state that must stay outside:
- Confidence:
- Evidence grade:
- Safety:
- Relationship notes:
- Migration:
- Validation:

## Lens 4: Code Health and Cleanup

### H1. <finding>
- Severity:
- Risk label:
- Symptom:
- Source:
- Consequence:
- Remedy:
- Cleanup disposition: <delete-now|keep-with-proof|remediate|needs-human-decision|n/a>
- Cleanup proof/evidence:
- Safety:
- Confidence:
- Evidence grade:
- Relationship notes:
- Validation:

### Prove-or-Delete Ledger
| Ledger ID | Files/tests | Suspicious item | Category | Disposition | Proof/evidence required or found | Validation protection | Registry link |
| --- | --- | --- | --- | --- | --- | --- | --- |

## Lens 5: Change-Set Quality Gate

### C1. <finding>
- Files:
- Change horizon:
- Structural regression:
- Missed simplification:
- Smallest correction:
- Confidence:
- Evidence grade:
- Safety:
- Relationship notes:
- Validation:

## Finding Registry

| ID | Lens | Files | Severity | Confidence | Evidence grade | Safety | Validation | Relationships | Cleanup disposition | Cleanup proof/evidence | Remediation path | Status |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |

## Cross-Lens Relationship Map

- Duplicates:
- Overlaps:
- Supports:
- Contradicts:
- Same root cause:
- Same remediation:

## Facet Index

- Files/modules:
- Severity:
- Confidence:
- Evidence grade:
- Safety:
- Validation:
- Root-cause/remediation cluster:

## Cross-Lens Synthesis

| Priority | Recommendation | Lens IDs | Risk | Safety | Why now |
| --- | --- | --- | --- | --- | --- |

## Remediation Paths

| Path | Finding IDs | Safety | Smallest first step | Validation |
| --- | --- | --- | --- | --- |

## Deferred / Watchlist Findings

| ID | Evidence | Reason deferred | Status | Revisit trigger |
| --- | --- | --- | --- | --- |

## Final Validation

| Check | Result | Confidence impact |
| --- | --- | --- |

## Not Reviewed / Continuation Ledger

| Scope omitted | Reason | Risk | Next validation step |
| --- | --- | --- | --- |

## Implementation-Ready Backlog
1. <implementation-ready next action>
```

Required output order:

1. Header: scope, confidence, lens execution, artifact paths.
2. Audit Preflight.
3. Five full lens reports in order: Module Topology and File-Size Balance, Deepening, Operational Mechanics, Code Health and Cleanup, Change-Set Quality.
4. Code Health prove-or-delete ledger.
5. Finding Registry.
6. Cross-Lens Relationship Map.
7. Facet Index.
8. Cross-Lens Synthesis.
9. Remediation Paths.
10. Deferred / Watchlist Findings.
11. Final Validation.
12. Not Reviewed / Continuation Ledger when needed.
13. Implementation-Ready Backlog.

## Rules

- Read-only by default.
- Do not route to other skills or external tools; this package must stand alone.
- Do not rely on hosted CI, cloud tools, external services, or web search to complete the audit.
- Do not impose concision, future-context, or "top findings only" limits on audit scope.
- Do not cap findings. Skip only items with no material maintainability, architecture, test, health, safety, or change-quality consequence.
- Keep topology/file-size/hierarchy/workspace-folder and related-file cluster inventory centralized in the Module Topology and File-Size Balance lens.
- Do not weaken the structural rules: >1000-code-line files, <200-code-line same-responsibility shards, 3+ related sibling-file clusters, dedicated-package stranded facade/sibling suspects, and flat hierarchy problems must still be surfaced by the topology lens with evidence.
- Root-level or parent-level facades must be audited hard.
- Compatibility is not a default excuse for deferral.
- Code Health owns the strict prove-or-delete ledger.
- Deletion recommendations must include protection evidence: caller/import search, tests, CLI behavior, documented public API review, and regression or characterization checks when needed.
- Tests are in cleanup scope.
- Treat broad but validated improvements as recommendable.
- Treat human policy judgment as the narrow gate for `needs-domain-decision`.
- Preserve ruthless scope. Lens separation reorganizes ownership; it must not delete critique categories, reporting detail, severity posture, audit techniques, perspectives, or remediation dimensions.
