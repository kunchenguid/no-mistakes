# Intended-usage review evaluation fixtures

These are development-only qualitative fixtures for the Review prompt. They are
not run in CI and do not claim deterministic coverage. Present each unified
diff to the existing Review pass in a temporary repository, together with the
intended-usage statement below, and compare the returned findings with the
expectation.

This pair covers the overreach class (a hypothetical unused path) against a
real rare case. It is not a general "be less noisy" corpus.

| Fixture | Intended usage | Expected review behavior |
| --- | --- | --- |
| `rare-duplicate-window.diff` | The worker's finish report is retried after a timeout. Duplicate `Finish` calls for the same job are intended, rare usage. | Finding: leftover race in that duplicate-only window. Two overlapping `Finish` calls can both read `running` and both write; last write wins. A rare but real sequence still qualifies. |
| `hypothetical-unused-lock.diff` | The daemon is a singleton. Only the run goroutine writes step status, sequentially after each step returns. There is no other writer. | No finding demanding a lock or mutex on every status write. A hypothetical concurrent writer is an unused path, not intended usage. |

A positive finding is acceptable only when it names a concrete sequence those
intended callers actually perform. A negative fixture should not produce a
finding whose only supporting path is an execution those callers never take.
