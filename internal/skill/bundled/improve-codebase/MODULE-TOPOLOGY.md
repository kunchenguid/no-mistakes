# Module Topology and File-Size Balance

Dedicated structural inventory lens for `improve-codebase`. This is the only lens that owns module topology, file/code-line size, hierarchy, workspace folder structure, related-file directory cluster detection, obsolete structural facades/package-home leftovers, and package/home recommendations.

Other lenses may cite this lens by finding ID when topology evidence affects depth, operational duplication, health risk, cleanup disposition, or change-set safety. They must not rerun or independently report this inventory.

## Output contract

Produce a standalone Module Topology and File-Size Balance Report. Use finding IDs `T1`, `T2`, etc. Keep each finding separate when it has distinct files, hierarchy smell, split/merge/home recommendation, safety profile, or validation path.

For each finding, include: files or directories, structural category, code-line evidence where relevant, hierarchy evidence, obsolete facade/package-home evidence when relevant, current responsibility mix, proposed module/submodule home, package/interface recommendation, confidence, evidence grade, safety, relationship notes, and validation.

## Inventory scope

Inventory every hand-authored source, test, config, and doc-support file in scope, excluding generated, vendor, build, cache, lock, raw data, artifact, and secret-bearing files.

Count code lines consistently enough to identify structural risk. State the counting method when exact counts matter. Do not weaponize line counts without reading responsibility boundaries.

## Structural families owned here

### Oversized split suspects

- Any hand-authored file over 1000 code lines is a mandatory split suspect.
- Report the responsibility mix, likely submodules, safest split order, interface surface, tests to preserve behavior, and validation path.
- If a >1000-line file should remain whole, keep it visible with concrete evidence. Use deferred-watchlist only when the strict registry blocker and revisit-trigger requirements are met.

### Undersized merge suspects

- Files under 200 code lines are merge suspects only when they are thin shards of the same feature, mechanic, domain, layer, or interface.
- Use names, imports, callers, tests, and domain docs as evidence.
- Do not merge independent concepts merely because they are small.

### Related-file directory clusters

- A directory with 3+ hand-authored sibling files that clearly belong to the same module/submodule is a hierarchy finding unless the directory itself is already dedicated to that exact domain.
- Once a dedicated package/directory exists for a domain, same-domain sibling files left beside it in the parent directory are `stranded facade` or `stranded sibling` suspects even when there are only 1-2 files. Example: if `analysis/training/` exists, parent-level `analysis/training_*.py` or equivalent same-domain leftovers are suspect even below the 3-file cluster threshold.
- The 3+ related-file cluster threshold is only a baseline for broad directories before a dedicated package exists. After the package exists, the question changes from "is there enough cluster mass?" to "why is this same-domain file still outside the package home?"
- Evidence may come from prefixes, concepts, import relationships, caller/test collaboration, or domain language.
- Examples include `training_*.py`, `gateway_*.py`, `model_*.py`, `codex_*.py`, `attachment_*.py`, paired source/test helper shards, or equivalent project-native naming.
- Recommend a real directory/package home such as `analysis/training/`, `analysis/gateway/`, `analysis/model/`, `analysis/codex/`, `analysis/attachment/`, or the project-native equivalent, with a small public interface.
- Do not apply this mechanically to coincidental prefixes, generated files, migrations, package entrypoints, or independent concepts that merely share a word.

### Stranded facade and stranded sibling audit

- Audit root-level or parent-level facades hard when a dedicated package already exists.
- For every same-domain file left beside its package home, require one disposition:
  - move real logic into the package;
  - reduce the facade to a tiny documented shim;
  - prove external compatibility requires the facade;
  - add a deprecation/migration plan with tests.
- Compatibility is not a default excuse for deferral. A compatibility facade can remain only when it is intentionally supported, tiny, documented, test-backed, and has a clear owner plus migration or long-term support policy.
- Do not accept a facade that recreates a flat cluster in another form. If a shim grows behavior, orchestration, branching, or hidden policy, report it as active topology work unless a genuine non-actionable-yet blocker exists.
- When the question is whether a facade, alias, old package home, or stranded sibling still needs to exist, record the structural evidence here and point Code Health and Cleanup to it for the prove-or-delete disposition. Do not duplicate the cleanup ledger here.

### Workspace folder structure and hierarchy

- Map hierarchy, not just package names.
- Flag flat monorepo-style layouts, broad dumping-ground folders, same-feature logic scattered across unrelated folders, missing domain/layer/sub-layer boundaries, and package sprawl that makes navigation non-hierarchical.
- Prefer tidy module -> submodule -> feature/layer homes such as `frontend/ux/forms`, `frontend/ui/homepage`, `backend/billing/invoices`, or project-native equivalents.
- Do not recommend a plain monorepo split when a hierarchical module/submodule structure is the real fix.

### Package/home recommendations

- Recommend package homes that improve locality, testability, navigability, and blast-radius control.
- Prefer a small public `__init__` or equivalent interface when the ecosystem supports it.
- Compatibility facades are allowed only when they preserve intentional public import surfaces or reduce migration risk in a documented, test-backed way. They must stay tiny, have a clear owner and migration/support policy, and must not recreate a permanent flat cluster of shims.
- If migration can be made now with characterization tests and local validators, classify the package-home recommendation as active or safe-cleanup rather than deferred-watchlist.

## Finding report card

```md
### T<N>. <structural finding>

- Files/directories:
- Structural category: <oversized split|undersized merge|related-file cluster|stranded facade|stranded sibling|flat hierarchy|scattered feature|workspace-folder structure>
- Code-line evidence:
- Hierarchy evidence:
- Obsolete facade/package-home evidence:
- Current responsibility mix:
- Proposed module/submodule home:
- Package/interface recommendation:
- Compatibility facade, if any:
- Required facade/sibling disposition:
- Confidence:
- Evidence grade:
- Safety:
- Relationship notes:
- Validation:
```

## Boundaries

- This lens owns structural inventory. Deepening owns depth/seams/locality/leverage/test surfaces. Operational Mechanics owns repeated operational mechanics. Code Health and Cleanup owns decay/debt/health/safety/test quality. Change-Set Quality owns local change/regression quality.
- Do not soften findings to avoid creating remediation work. Evidence-backed structural imperfection must be reported as active, safe-cleanup, deferred-watchlist, or needs-domain-decision.
- Prefer active or safe-cleanup for structural improvements that can be made now with characterization tests and local validators, even when they are broad moves, module migrations, compatibility deprecations, or test-topology changes.
- Use deferred-watchlist only when the structural issue is genuinely non-actionable yet and has concrete evidence plus a concrete revisit trigger. Large scope, risk, compatibility, inconvenience, low priority, or "could be improved later" are not valid deferral reasons.
- Use needs-domain-decision only when the remedy requires a human product, security, business, ownership, or domain policy decision that cannot be inferred from code. Code structure, refactor size, compatibility concern, and architectural preference are usually not enough.
- Do not delete critique scope. If a structural concern intersects another lens, keep the structural fact here and cross-reference it elsewhere during synthesis.
