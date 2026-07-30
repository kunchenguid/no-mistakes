# Test-quality guidance validation

Validated target: `0dabd46b25c1919c493d3193630d0a862a3e4e93`

## Reviewer-visible task-first surface

The generated `skills/no-mistakes/SKILL.md` now tells task-first agents:

> Never add a test whose only evidence is that it opens, reads, greps, parses, or snapshots implementation source code and finds or omits particular strings, tokens, lines, commands, function names, prompt phrases, regex matches, AST shapes, or incidental snapshots.

It directs agents to execute public or executable interfaces and assert observable behavior. It also preserves these explicit exceptions:

- Machine-consumed declarative files may be parsed into a typed or normalized semantic model to assert meaning.
- Generated public output, serialized protocols, persisted state, intentional snapshots, and explicitly owned text or byte contracts may be read as the contract under test.
- Deterministic CI may inspect the final emitted agent prompt as an intentional generated interface.
- Model interpretation remains a development-only evaluation rather than live-LLM CI.

`go run ./cmd/genskill --check` confirmed that this committed skill is the current generated output.

## Emitted role matrix exercised

| Agent surface | Focused contract exercised |
| --- | --- |
| Task-first skill | Generated skill contains the shared rule |
| Initial review | Shared rule plus reviewer enforcement and no-repository-wide-cleanup boundary |
| Review repair and rereview | Shared rule survives the finding, repair, and clean rereview flow |
| Initial test | Shared rule is in the actual evidence-agent prompt |
| Test repair | Shared rule is in the failing-test repair prompt |
| CI repair | Shared rule is in the late CI auto-fix prompt |
| Rebase repair | Shared rule is in the late conflict-repair prompt |

The deterministic fake-agent journey returned a source-content-only finding, entered the ordinary auto-fix gate, performed the repair turn, and accepted the clean rereview. No live model was used in CI.

## Development-only qualitative evaluation

`CONTRIBUTING.md` records the development-only synthetic evaluation: a staged Go test that only searched `app.go` for a function-signature token was flagged even though the implementation returned the wrong behavior, while a typed semantic assertion over `policy.json` was accepted. The record explicitly identifies this as qualitative policy evidence, not a live-LLM CI gate.
