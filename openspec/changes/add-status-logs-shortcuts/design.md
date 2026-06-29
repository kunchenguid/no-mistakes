## Context

`axi status` and `axi logs` live under the agent-facing `axi` group (`internal/cli/axi_query.go`).
Per the repo conventions, the live `axi` strings are an agent-guidance surface that must stay in sync with the skill body and `docs/.../agents.md`, so the `axi` command names cannot be renamed or repurposed.
There is already a separate top-level `status` command (`internal/cli/status.go`) that prints a styled human repository summary; it is unrelated to the TOON run view and must keep working.
The render logic for the TOON views is self-contained in `runAxiStatus(cmd, runID)` and `runAxiLogs(cmd, step, runID, full)`, each driven only by a `*cobra.Command` and its flags.

## Goals / Non-Goals

**Goals:**
- Let a human type `st` and `lg` (alias `logs`) at the top level to get the exact `axi status` / `axi logs` output.
- Guarantee the shortcut and the `axi` subcommand cannot drift, by building both from one source.
- Keep human and agent telemetry distinguishable.

**Non-Goals:**
- The `nm` binary alias (shell/dotfiles concern, out of scope).
- Any change to `axi` command behavior, the human `status` command, run semantics, IPC, or the daemon.
- Adding the shortcuts to the agent skill or `agents.md`.
- Cobra prefix-matching or top-level `s`/`l` single-letter aliases (avoids ambiguity with `status`).

## Decisions

### Shared command builder over copied command definitions

Factor the flag set and `RunE` body of each command into a single constructor parameterized by the command name, `Short` string, and telemetry surface:

- `newRunStatusCommand(use, short, surface)` → registered once under `axi` as `status`, once at root as `st`.
- `newRunLogsCommand(use, short, surface, aliases)` → registered once under `axi` as `logs`, once at root as `lg` (with alias `logs`).

The body still calls `runAxiStatus` / `runAxiLogs` unchanged. Only the cobra wrapper is shared.

Alternative considered: cobra `Aliases` on the existing commands. Rejected — cobra aliases are same-level synonyms and cannot project an `axi` subcommand to the top level.

Alternative considered: duplicate top-level command definitions that call the same render functions. Rejected — two hand-maintained flag sets drift (a new `axi logs` flag would silently miss `lg`).

### Distinct telemetry surface per shortcut

The `axi` commands wrap their body in `trackAxiSurface("axi-status", "/axi/status", …)`. The shortcut call sites pass distinct identifiers (`"st"` / `"/st"`, `"lg"` / `"/lg"`) so the agent-driving analytics are not polluted by human shortcut usage and the feature's adoption is measurable.

### `lg` as the command, `logs` as its alias

No top-level `logs` exists today, so `lg` is the canonical name (shortest to type) with cobra `Aliases: ["logs"]` for discoverability by the full word. `status` is already taken by the human command, so the status shortcut is only `st` (no `status` alias, no single-letter `s`).

### Docs in cli.md only

Add `st` / `lg` sections to `docs/reference/cli.md`. Do not touch `internal/skill/skill.go` or `docs/.../agents.md`; `make lint` runs `skill-check` and would fail on drift, which doubles as the guard that the agent contract stayed frozen.

## Risks / Trade-offs

- [Two status-like commands one keystroke apart (`status` vs `st`)] → Distinct, accurate `Short` strings disambiguate them in `--help`; cobra does not prefix-match by default, so neither shadows the other.
- [Shared builder over-couples the two call sites] → The shared part is only the cobra wrapper (flags + RunE delegation); the render functions stay independent, so the coupling is exactly the contract we want to keep identical.
- [Help output gains two more top-level commands] → Acceptable; clear `Short` strings keep the list readable, and `lg`'s `logs` alias aids discovery.
