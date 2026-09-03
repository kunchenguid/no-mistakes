# Simplification review evaluation fixtures

These are development-only qualitative fixtures for the Review prompt's
dedicated Simplification section. Their qualitative agent evaluation is not run
in CI and does not claim deterministic coverage; regular tests only verify that
the unified diffs remain applicable. Present each diff to the existing Review pass
in a temporary repository, together with the intent below as `--intent`, and
compare the returned findings with the expectation.

The set is modeled on a real review-fix spiral (backpass PR #107): a
permissive target resolver and a second, skill-only budget semantics were
each hardened round after round because every round's defect was concrete and
reachable, when the intent required neither component and one deletion would
have closed the whole class.

| Fixture | Intent | Expected review behavior |
| --- | --- | --- |
| `permissive-target-resolver.diff` | `--target X` selects exactly one loaded skill by its name, or one configured memory file by its configured path. An unknown or ambiguous target errors loudly. | Simplification warnings (`ask-user`) naming the skill path/basename branch, the any-`.md`-under-`skills/` branch, the memory-file basename branch, and the any-existing-file branch as components the intent does not require, each with removal as the remedy. A defect finding that `--target README.md` is accepted as a memory file must name removing the any-existing-file branch as the remedy, not adding a filter in front of it. |
| `second-budget-semantics.diff` | A skill target trains only that skill's `SKILL.md`. The token budget rule is unchanged. | Simplification warning (`ask-user`) naming the skill-only budget branch and its `AlwaysLoadedTokens` side channel as a second definition of a concept the code already defines once, with removal as the remedy. No finding asking to make the second budget consistent elsewhere. |
| `exact-match-resolver.diff` | Same intent as the permissive fixture. | No Simplification finding: every component is strictly required by the intent. |

A Simplification finding is acceptable only when it names the component,
states that no intent requirement needs it, and recommends removal. It must
never recommend hardening, validating, or documenting the unrequired component.
