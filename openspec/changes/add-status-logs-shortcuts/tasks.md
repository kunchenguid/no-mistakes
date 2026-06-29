## 1. Shared command builders

- [x] 1.1 In `internal/cli/axi_query.go`, factor the `status` cobra command into `newRunStatusCommand(use, short, surface, path string)` that wires the `--run` flag and the telemetry-wrapped `runAxiStatus` body.
- [x] 1.2 Factor the `logs` cobra command into `newRunLogsCommand(use, short, surface, path string, aliases []string)` that wires `--step`, `--run`, `--full` and the telemetry-wrapped `runAxiLogs` body.
- [x] 1.3 Rebuild `newAxiStatusCmd` / `newAxiLogsCmd` on top of the new builders, passing the existing `axi-status`/`/axi/status` and `axi-logs`/`/axi/logs` surfaces, so the `axi` output and telemetry are byte-for-byte unchanged.

## 2. Top-level shortcut commands

- [x] 2.1 Add `newStShortcutCmd()` using `newRunStatusCommand("st", <human short>, "st", "/st")`.
- [x] 2.2 Add `newLgShortcutCmd()` using `newRunLogsCommand("lg", <human short>, "lg", "/lg", []string{"logs"})`.
- [x] 2.3 Register both on the root command in `internal/cli/root.go` (alongside, not replacing, the existing `status` command).

## 3. Tests

- [x] 3.1 Add CLI tests asserting `st` output matches `axi status` (active run, `--run <id>`, and no-runs cases) and exit codes match.
- [x] 3.2 Add CLI tests asserting `lg` and its `logs` alias match `axi logs` for `--step`, `--full`, and the missing-`--step` exit-code-2 case.
- [x] 3.3 Add a test asserting `st`/`lg` record their own telemetry surfaces (`st`/`lg`), not `axi-status`/`axi-logs`.
- [x] 3.4 Assert the existing top-level `status` command output is unchanged.

## 4. Docs and verification

- [x] 4.1 Add `st` and `lg` sections to `docs/reference/cli.md`; do not touch `internal/skill/skill.go` or `docs/.../agents.md`.
- [x] 4.2 Run `make lint` (skill drift check must stay green, confirming the agent contract was not changed) and `go test -race ./internal/cli`.
- [x] 4.3 Relied on package tests per repo guidance: the shortcuts delegate to the identical `runAxiStatus`/`runAxiLogs` render path e2e already covers, adding no new process or I/O boundary, and the CLI tests assert byte-for-byte output parity with `axi status`/`axi logs`.
