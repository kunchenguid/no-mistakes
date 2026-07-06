# Finding Registry

Use the registry as the complete ledger for an `improve-codebase` audit. Synthesis, facets, and remediation paths are navigation over this ledger, not replacements for it. Do not hide an evidence-backed finding because it is lower priority, overlaps another finding, or is deferred.

Read this file before assigning evidence grades, cleanup dispositions, relationships, or statuses. Do not invent status meanings in a lens report.

## Required fields

Every lens finding must be convertible into one registry row with these fields:

- `ID`: primary lens ID, preserving prefixes `T*`, `D*`, `O*`, `H*`, or `C*`.
- `Lens`: Module Topology and File-Size Balance, Deepening, Operational Mechanics, Code Health and Cleanup, or Change-Set Quality.
- `Files`: concrete files, modules, tests, docs, or "repo-wide" when the evidence is systemic.
- `Severity`: Critical, Warning, Suggestion, or Info. Preserve Code Health severity when available.
- `Confidence`: high, medium, or low, with a short reason.
- `Evidence grade`: one of the grades below.
- `Safety`: Safe, Extended-Safe, Residual, or Unknown.
- `Validation`: command, test, review step, or "needs validation discovery".
- `Relationships`: relationship type plus target finding IDs, or "none observed".
- `Cleanup disposition`: for Code Health prove-or-delete items, one of `delete-now`, `keep-with-proof`, `remediate`, or `needs-human-decision`; use `n/a` for findings outside that ledger.
- `Cleanup proof/evidence`: proof that justifies the disposition, or the missing proof that makes the cleanup actionable; use `n/a` outside the ledger.
- `Remediation path`: smallest credible implementation path or "watch only until <revisit trigger>".
- `Status`: one of the statuses below.

## Prove-or-delete cleanup fields

Code Health and Cleanup owns the strict cleanup ledger for legacy/dead/unnecessary/speculative/compatibility-smell/over-engineered production and test code. Registry rows linked to that ledger must record disposition and proof:

- `delete-now`: caller/import search, tests, CLI/import behavior, public API review, and regression or characterization checks show the item can be removed without breaking current functionality.
- `keep-with-proof`: concrete public compatibility, external contract, documented CLI/import, downstream caller, or active migration-window evidence justifies keeping it. Vague "legacy" or "compatibility" wording is not proof.
- `remediate`: current behavior remains useful, but the legacy wrapper, pass-through helper, stale migration path, speculative seam/adapter/option, or over-engineered test helper should be replaced, simplified, inlined, or moved behind the current module/package home.
- `needs-human-decision`: reserved for real product/API compatibility commitments that cannot be inferred from repository evidence.

Tests are in scope. Test helper compatibility layers, broad fixtures, patch helpers, fake adapters, and tests of old facades need the same disposition/proof discipline as production code.

## Evidence grades

- `direct-source`: direct source-code, config, docs, or command-output evidence.
- `test-evidence`: failing/passing tests, coverage evidence, suite behavior, or test gap tied to changed behavior.
- `structural-inference`: module graph, coupling, size, layering, fan-out, duplication, or interface-shape inference.
- `historical-change`: git history, branch diff, recent commits, churn, or touched-area evidence.
- `speculative-plausible`: plausible concern with incomplete proof; keep confidence low and do not over-prioritize.

Prefer the strongest honest grade. A finding may mention supporting grades in notes, but the registry field should use one primary grade.

## Relationship types

- `duplicates`: substantially the same finding as another lens item.
- `overlaps`: shares files, symptoms, or remediation but is not the same finding.
- `supports`: strengthens another finding's evidence or priority.
- `contradicts`: conflicts with another finding or suggests a different remedy.
- `same-root-cause`: different symptoms likely caused by the same structural problem.
- `same-remediation`: separate findings that can be addressed by the same implementation path.

Relationships must cite target finding IDs. Use `none observed` when no relationship is found; do not invent clusters.

Contradictions must be explicit. If two lens findings conflict on diagnosis, remedy, safety, status, or deletion disposition, record a `contradicts` relationship on the affected rows. The final synthesis must either resolve the conflict with cited evidence or lower confidence and keep the contradiction visible. Do not silently choose one lens over another.

## Statuses

- `active`: evidence-backed and ready to consider for remediation now. This includes large refactors, structural moves, logic/system changes, module migrations, compatibility deprecations, and test-topology changes when they improve the codebase and can be validated.
- `deferred-watchlist`: evidence-backed but genuinely non-actionable yet, with a concrete blocker and a concrete revisit trigger. Valid blockers include missing platform support, unowned external policy, unavailable runtime/hardware, or a future trigger that does not exist yet. Large scope, risk, compatibility, inconvenience, low priority, or "could be improved later" are not valid deferral reasons.
- `needs-domain-decision`: remedy requires a human product, security, business, ownership, or domain policy decision that cannot be inferred from code and cannot be made safely by an improvement agent. Code structure, refactor size, compatibility concern, and architectural preference are usually not enough.
- `blocked-by-missing-tests`: remedy is credible, but safe execution needs missing characterization or integration tests that the improvement agent cannot reasonably create locally. If the agent can add characterization tests and run local validators, use `active` or `safe-cleanup` instead.
- `safe-cleanup`: behavior-preserving cleanup with clear validation. It may span files or modules when test-backed and compatibility-preserving; it is not limited to tiny edits.

Deferred statuses still stay in the registry and the Deferred / Watchlist Findings section with evidence and the reason for deferral.

## Status selection rules

- Default evidence-backed, locally actionable improvements to `active` or `safe-cleanup`.
- Default unproven legacy, dead, unnecessary, compatibility-smell, or over-engineered code/tests to an actionable cleanup row. It must become `delete-now`, `remediate`, or `needs-human-decision`; it may be `keep-with-proof` only with concrete compatibility or migration evidence.
- Do not use `deferred-watchlist` merely because remediation is broad, risky, compatibility-sensitive, inconvenient, not urgent, or easier to postpone.
- Do not use `needs-domain-decision` for ordinary code organization choices. Propose an improvement path with characterization tests, compatibility tests, or deprecation/migration steps instead.
- Use `needs-domain-decision` only for policy judgment outside the codebase, such as product behavior, security posture, business ownership, data governance, or external contractual compatibility that the agent cannot decide.
- Root-level or parent-level compatibility facades are `active` topology suspects unless they are intentionally supported, tiny, documented, test-backed, and governed by a clear owner plus migration or support policy.

## Registry table

Use this compact shape unless the audit needs a wider artifact table:

```md
| ID | Lens | Files | Severity | Confidence | Evidence grade | Safety | Validation | Relationships | Cleanup disposition | Cleanup proof/evidence | Remediation path | Status |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| D1 | Deepening | `src/a.ts` | Warning | high - callers show leakage | structural-inference | Residual | `npm test` | supports H2 | n/a | n/a | Deepen parser boundary | active |
```
