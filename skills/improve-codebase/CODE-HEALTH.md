# Code Health and Cleanup

Required standalone health, debt, test-quality, cleanup, and safety lens for
every `improve-codebase` audit.

## Ground rules

- Output a full standalone Code Health and Cleanup section inside the unified audit.
- Use finding IDs `H1`, `H2`, etc. as the primary IDs for health registry rows.
- Every health finding must follow: `Symptom -> Source -> Consequence -> Remedy`.
- Treat thresholds as hints, not verdicts. Check context, intent, ownership, tradeoffs, and blast radius before calling something debt.
- Prefer concrete architecture, maintainability, or test consequences over style complaints.
- Write remedies using this package's vocabulary: **module**, **interface**, **seam**, **adapter**, **depth**, **locality**, **leverage**, and **shared-mechanics module**.
- This lens owns the strict prove-or-delete ledger for legacy, dead, unnecessary, speculative, compatibility-smell, and over-engineered production/test code.
- The lens is read-only. Classify remedy safety, but do not edit files.

## Strict prove-or-delete ledger

Maintain a ledger for production and test code. Every suspicious item receives
exactly one disposition:

- `delete-now`: evidence shows the item is unused, obsolete, redundant, or only protects legacy noise, and it can be removed with validation.
- `keep-with-proof`: keep only with concrete evidence of public compatibility, external contract, documented CLI/import surface, or an active migration window. The proof must name the contract, caller, owner, or migration endpoint.
- `remediate`: replace, simplify, inline, collapse, or move behind the current package/module home because the current shape is too indirect or stale but some current behavior remains useful.
- `needs-human-decision`: use only for real product/API compatibility decisions that cannot be inferred from repo evidence.

Suspicious item classes:

- legacy shims, facades, aliases, and old package homes;
- compatibility wrappers and dual-path adapters;
- dead or unreferenced functions, classes, modules, files, exports, fixtures, and helper APIs;
- unnecessary pass-through helpers that add names, mocks, or branches without current caller value;
- stale migration paths, transitional flags, fallback branches, old import paths, and retired data-shape handling;
- speculative abstractions, seams, adapters, options, and extension points without at least one current caller benefit;
- over-engineered test helper layers, broad fixtures, patch helpers, fake adapters, and mock scaffolds;
- tests that only protect old facades, aliases, compatibility wrappers, or implementation noise instead of current behavior.

Compatibility and legacy are non-neutral. If compatibility value is claimed,
prove it with concrete public API, CLI/import, external contract, documented
downstream caller, or migration-window evidence. If proof is missing, mark the
item `delete-now` when safe, `remediate` when current behavior must
move/simplify, or `needs-human-decision` only when the compatibility commitment
is a real external product/API decision.

Deletion and remediation need validation protection before being recommended as safe:

- caller/import/reference search across source, tests, configs, docs, and package exports;
- tests that exercise current behavior, plus regression or characterization tests when behavior is risky or under-specified;
- CLI/import behavior checks for exported commands, public modules, package entrypoints, and documented examples;
- documented public API and migration-window review;
- local validators that would fail if main functionality broke.

Relationship to other lenses:

- Deepening supports this ledger by flagging over-engineered shallow abstractions, weak seams, speculative ports/adapters/options, and test surfaces that lock in implementation noise.
- Module Topology supports this ledger by flagging obsolete facades, stranded siblings, and package-home leftovers when they are structural.
- Operational Mechanics may identify repeated mechanics that make a helper useful. If so, record `keep-with-proof` or `remediate` with the operational finding ID.

## Decay risks

Use these diagnostic labels for health findings:

- **Cognitive Overload**: too much effort to understand. Signals: long routines, deep nesting, unclear names, magic values, train-wreck chains, flag args, primitive obsession, shallow modules.
- **Change Propagation**: one change ripples through unrelated modules. Signals: shotgun surgery, divergent change, feature envy, inappropriate intimacy, hidden observable contracts, information leakage, Hyrum's Law, orthogonality violations.
- **Knowledge Duplication**: same decision expressed in multiple places. Signals: duplicate logic, inconsistent names for the same concept, parallel hierarchies, repeated constants, independent implementations of the same algorithm.
- **Accidental Complexity**: code is more complex than the problem. Signals: speculative abstractions, lazy modules, middle-men, unused extension points, second-system effect, tactical programming debt.
- **Dependency Disorder**: dependencies do not flow in a predictable direction. Signals: cycles, domain importing infrastructure, unstable modules imported by stable modules, fat interfaces, fan-out hotspots, diamond dependency or upgrade blockage, conceptual inconsistency.
- **Domain Model Distortion**: code does not match the problem language. Signals: anemic domain objects where behavior is expected, business logic in vague layers, bounded context crossings without translation, LSP breaks, value objects modeled as mutable entities, domain names that do not match stakeholder language.

## Health lenses

Apply all relevant sub-lenses and report enough detail for the section to stand alone.

### Local/recent code decay

Use when a diff, changed files, or recent local commits exist.

- Determine scope from staged diff, unstaged diff, branch diff, recent commits, or user-supplied range when available.
- Scan changed production code for the six decay risks.
- Add a quick test check: new behavior without tests, mock-heavy tests, unclear test names.
- Skip generated files, lockfiles, migrations, and vendored code unless the user explicitly asks.

### Architecture health

Use for every audit.

- Map dependency direction, layering direction, circular dependencies, conceptual integrity, and architecture consequences of decay risks.
- Use Module Topology finding IDs when structural inventory affects risk, debt priority, health score, or cleanup safety.
- Do not independently inventory file sizes, hierarchy, workspace-folder structure, or related-file directory clusters.
- When an obsolete facade or package-home leftover is structural, cite the topology finding ID and add the cleanup disposition in the prove-or-delete ledger.
- Include a module dependency graph when there is enough structure to draw one honestly.
- Check layering direction, circular dependencies, fan-out > 5, seam density at infrastructure edges, and conceptual integrity.
- Run an ownership-alignment check only when ownership is known. If unknown, write `ownership alignment: skipped; ownership unknown`.

### Tech debt

- Score each systemic finding with Pain (1-3) x Spread (1-3).
- Classify 7-9 as Critical debt, 4-6 as Scheduled debt, 1-3 as Monitored debt.
- Mark each item `[intentional]` only when the shortcut is visible as a deliberate tradeoff with an owner or payback plan. Otherwise mark `[accidental]`.
- Prefer fixing accidental high-priority debt before intentional debt with clear ownership.

### Test quality

Use when test files exist or production behavior implies tests should exist.

- Build a compact suite map when possible: unit, integration, e2e, coverage gaps.
- Scan for Test Obscurity, Test Brittleness, Test Duplication, Mock Abuse, Coverage Illusion, and Architecture Mismatch.
- Include tests in the prove-or-delete ledger when they exist only to preserve old facades, compatibility wrappers, fake adapter layers, broad fixtures, patch helpers, or implementation details that no longer represent current behavior.
- Treat missing characterization tests around actively changed legacy code as a serious change-safety problem.
- Treat slow suites as architecture evidence: >10 minutes is at least a Warning; >30 minutes or unknown runtime in a relied-on suite may be Critical.
- Recommend tests at the module interface or shared operational interface; do not suggest tests that lock in implementation details.

### Health dashboard

- Score Architecture, Tech Debt, and Test Quality from 100 with deductions: Critical -15, Warning -5, Suggestion -1.
- Add Code Quality score only when changed files exist.
- Composite score is a weighted average: Code Quality 0.25, Architecture 0.30, Tech Debt 0.25, Test Quality 0.20.
- If no changed files exist, redistribute the Code Quality weight across the remaining dimensions proportionally.
- Keep health scoring tied to observed findings; scores summarize risk and should not erase distinct findings.

### Cleanup safety

Use for remediation classification only.

- **Safe**: single-file, local, non-exported, behavior-preserving cleanup.
- **Extended-Safe**: touches <= 5 files, a project test command exists and passes before the change, and there is no public interface/signature/exported-symbol change.
- **Residual**: public interface change, module layout change, cross-service change, no tests, ambiguous remedy, repeated failed fix attempt, or any refactor needing product/domain judgment.

Safety is not status. Residual or Extended-Safe findings can still be `active`
when the improvement is evidence-backed and has a credible validation path.

## Report template

````md
## Code Health and Cleanup

**Health sub-lenses:** local/recent code decay, architecture health, tech debt, test quality, health dashboard, cleanup safety
**Scope:** <files/modules reviewed>
**Composite health:** <score or "not scored">

### Findings

#### Critical

**H<N>. <Risk Label> - <short title>**
Symptom: <observed evidence>
Source: <local source, test, config, docs, or structural evidence>
Consequence: <architecture or maintainability harm>
Remedy: <specific remedy using improve-codebase vocabulary>
Cleanup disposition: <delete-now|keep-with-proof|remediate|needs-human-decision|n/a>
Cleanup proof/evidence: <caller/import/API/migration/test evidence or missing proof that makes it actionable>
Safety: <Safe | Extended-Safe | Residual>
Confidence: <high|medium|low and why>
Evidence grade: <direct-source|test-evidence|structural-inference|historical-change|speculative-plausible>
Relationship notes: <duplicates|overlaps|supports|contradicts|same-root-cause|same-remediation target IDs, or none observed>
Validation: <command or validation discovery needed>

#### Warning

...

#### Suggestion

...

### Debt and Test Notes

- Debt priority: <Pain x Spread and intentional/accidental notes>
- Test quality: <suite map, coverage gaps, or "no test evidence reviewed">

### Prove-or-Delete Ledger

| Ledger ID | Files/tests | Suspicious item | Category | Disposition | Proof/evidence required or found | Validation protection | Registry link |
| --- | --- | --- | --- | --- | --- | --- | --- |
````
