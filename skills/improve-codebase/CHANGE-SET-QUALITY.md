# Change-Set Quality Gate

Run a strict maintainability review of work that may already be committed, staged, unstaged, or spread across a touched area. This lens is not a normal PR review and not a duplicate whole codebase architecture audit. Its job is to answer: **did recent or local work make the codebase harder to change than it needed to?**

## Scope Priority

Start by building a deterministic change horizon. Run these commands from the repo root when available:

```bash
git status --short
git diff --stat
git diff --cached --stat
git rev-parse --abbrev-ref --symbolic-full-name @{u}
git merge-base HEAD @{u}
git diff --name-only <merge-base>..HEAD
```

If there is no upstream branch or the upstream comparison fails, fall back to recent commits with commands such as `git log --oneline --decorate -n 10`, `git diff --name-only HEAD~5..HEAD`, or a user-supplied range. State the fallback and lower confidence when the horizon is weaker.

Prefer evidence in this order:

1. **Local work inventory**
   - `git status --short`
   - `git diff --stat`
   - `git diff --cached --stat`
   - local commits ahead of the upstream branch from the merge-base comparison
   - recent commits when no upstream comparison is reliable
   - any user-supplied range or issue/PR scope

2. **Touched-area audit**
   - For changed or recently touched files, inspect neighboring modules, callers, tests, and ownership boundaries.
   - Report pre-existing debt only when the local work touches, expands, depends on, or worsens it.
   - Prefer "this change landed in the wrong place" over generic complaints about the surrounding code.

3. **Fallback high-risk-area scan**
   - If there is no reliable change horizon, inspect the largest, busiest, most recently modified, most coupled, or hardest-to-test files in scope.
   - Mark confidence lower when findings are high-risk-area based rather than tied to local work.

Branch diffs are useful evidence, but do not let "current diff only" define the lens. A giant feature may already be committed. A refactor may span many commits. A dirty working tree may only show the last scratch edits.

## What To Flag

Escalate high-conviction maintainability regressions:

- A complicated implementation where a smaller reframing could delete branches, helpers, modes, or layers.
- Refactors that move complexity around without reducing the concepts a maintainer must hold.
- Local work that worsens or ignores existing Module Topology and File-Size Balance findings. Cross-reference the topology finding IDs rather than rerunning file-size, hierarchy, workspace-folder, or related-file cluster inventory.
- New ad-hoc conditionals, one-off flags, nullable modes, or special cases inside already busy flows.
- Feature-specific logic leaking into shared paths or shared logic leaking through an interface.
- Generic magic that hides simple data-shape assumptions.
- Thin wrappers, identity helpers, or pass-through abstractions that add indirection without leverage.
- Cast-heavy, optional-heavy, `any`, `unknown`, or loosely shaped contracts that obscure the real invariant.
- Bespoke helpers where a canonical local utility already exists.
- Sequential orchestration or partial-update flow that makes state harder to reason about when a cleaner structure is obvious.

Avoid low-value nits. Report every material evidence-backed maintainability regression. Skip only style-only nits with no structural consequence.

## Review Questions

For each meaningful local or touched-area change, ask:

- What complexity did this work add or preserve?
- Was that complexity necessary for the requested behavior?
- Is the logic in the canonical layer, module, or package?
- Did the work increase branching, coupling, statefulness, file size, type looseness, wrapper layers, or orchestration fragility?
- Did the work worsen, ignore, or add risk around a topology finding reported by the dedicated Module Topology and File-Size Balance lens?
- If the touched area depends on hierarchy or package-home shape, is that evidence cross-referenced to the topology lens instead of reinvented here?
- Could a simpler model make branches or helper layers disappear?
- Did the implementation reuse existing local helpers and patterns?
- Did local work worsen, fail to address, or depend on a topology finding already reported by the dedicated Module Topology and File-Size Balance lens?
- What is the smallest correction that preserves behavior and improves maintainability?

## Finding Shape

Use finding IDs `C1`, `C2`, etc.

```md
### C1. <finding>
- Files:
- Change horizon: <unstaged|staged|local commits|recent commits|touched area|high-risk area>
- Structural regression:
- Missed simplification:
- Smallest correction:
- Confidence: <high|medium|low and why>
- Evidence grade: <direct-source|test-evidence|structural-inference|historical-change|speculative-plausible>
- Safety: <Safe|Extended-Safe|Residual|Unknown>
- Relationship notes:
- Validation:
```

## Boundaries

- Do not repeat broad Deepening, Operational Mechanics, or Code Health findings unless the local work specifically worsens them.
- Do not require a git diff to exist.
- Do not approve an implementation merely because behavior appears correct.
- Do not propose speculative abstraction. The correction must delete real complexity, move logic to an existing canonical home, or introduce a seam that is already justified by callers/tests.
