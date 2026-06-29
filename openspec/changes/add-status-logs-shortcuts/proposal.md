## Why

Checking the status and step logs of a pipeline run is the most frequent thing a human does at the terminal, yet it requires typing the full `no-mistakes axi status` / `no-mistakes axi logs --step <name>` every time.
The `axi` group is the agent surface and must keep its exact command names for the agent contract, so the human shortcut has to be added alongside it rather than by renaming it.

## What Changes

- Add a top-level `st` command that prints the same dense TOON run view as `axi status` (supports `--run <id>`).
- Add a top-level `lg` command (alias `logs`) that prints the same step logs as `axi logs` (supports `--step`, `--run`, `--full`).
- Both delegate to the existing `axi status` / `axi logs` render functions through a single shared command builder, so the two call sites cannot drift.
- The shortcuts emit telemetry under their own surface names (distinct from the `axi-*` surfaces) so human shortcut usage is measurable separately from agent driving.
- The existing human `no-mistakes status` (styled one-shot repo summary) is unchanged and continues to coexist with `st`.
- Document the shortcuts in `docs/reference/cli.md` only. They are deliberately **not** added to the agent skill body or `docs/.../agents.md`, so the agent driving contract stays frozen on `axi status` / `axi logs`.
- The `nm` shell alias for the binary is a personal dotfiles concern and is explicitly out of scope here.

## Capabilities

### New Capabilities
- `cli-run-shortcuts`: Top-level human shortcut commands (`st`, `lg`) that expose the dense run status and step-log views without the `axi` prefix, sharing their implementation and contract with the `axi` subcommands.

### Modified Capabilities
<!-- None. The axi commands and the human `status` command keep their existing behavior; this change only adds new top-level commands. -->

## Impact

- `internal/cli`: new top-level commands registered on the root command (`root.go`); a shared builder factored out of `axi_query.go` so `axi status`/`axi logs` and `st`/`lg` are constructed from one source.
- Telemetry: two new surface identifiers for the shortcut commands.
- `docs/reference/cli.md`: new sections for `st` and `lg`.
- No change to the agent skill (`internal/skill/skill.go`), `docs/.../agents.md`, the IPC layer, the daemon, or run semantics.
