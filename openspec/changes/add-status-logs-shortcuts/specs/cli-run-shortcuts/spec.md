## ADDED Requirements

### Requirement: Top-level `st` status shortcut

The CLI SHALL provide a top-level `st` command that produces output identical to `axi status`, including its TOON document, exit codes (0 success/no-op, 1 error, 2 usage), and structured error rendering.
The `st` command SHALL accept the same `--run <id>` flag as `axi status` with the same default (active or most recent run).

#### Scenario: Shows the active run

- **WHEN** a user runs `no-mistakes st` in a repository with an active run
- **THEN** the command prints the same `run:` TOON object, gate fields, and help that `no-mistakes axi status` prints for that run

#### Scenario: Inspects a specific run

- **WHEN** a user runs `no-mistakes st --run <id>`
- **THEN** the command renders that run exactly as `no-mistakes axi status --run <id>` would

#### Scenario: No run exists

- **WHEN** a user runs `no-mistakes st` in a repository with no runs
- **THEN** the command prints the same "0 runs yet" document and start-run help that `axi status` prints, and exits 0

### Requirement: Top-level `lg` logs shortcut

The CLI SHALL provide a top-level `lg` command, with `logs` as an alias, that produces output identical to `axi logs`.
The `lg` command SHALL accept the same `--step`, `--run`, and `--full` flags with the same semantics, including requiring `--step` and rejecting an unknown step with exit code 2.

#### Scenario: Shows step logs

- **WHEN** a user runs `no-mistakes lg --step review`
- **THEN** the command prints the same tail-of-log output that `no-mistakes axi logs --step review` prints

#### Scenario: Full logs via the alias

- **WHEN** a user runs `no-mistakes logs --step ci --full`
- **THEN** the command prints the entire log for the `ci` step, identical to `no-mistakes axi logs --step ci --full`

#### Scenario: Missing required step

- **WHEN** a user runs `no-mistakes lg` without `--step`
- **THEN** the command exits with code 2 and the same "--step is required" guidance that `axi logs` emits

### Requirement: Human and agent surfaces remain distinct

Adding the shortcuts SHALL NOT change the `axi status` or `axi logs` commands, and SHALL NOT change the existing top-level `status` command (the styled human repository summary).
The `st` and `lg` shortcuts SHALL emit telemetry under surface identifiers distinct from the `axi-status` and `axi-logs` surfaces so human shortcut usage is measurable separately from agent driving.

#### Scenario: Existing status command is unaffected

- **WHEN** a user runs `no-mistakes status`
- **THEN** the command prints the existing styled repository summary, not the TOON run view

#### Scenario: Shortcut telemetry is attributed separately

- **WHEN** a user runs `no-mistakes st` or `no-mistakes lg`
- **THEN** the recorded telemetry surface is the shortcut surface, not the `axi-status` or `axi-logs` surface

### Requirement: Shortcuts excluded from the agent driving contract

The `st` and `lg` shortcuts SHALL be documented for humans in the CLI reference only.
They SHALL NOT be referenced in the generated agent skill body or the agent driving guide, which continue to instruct agents to use `axi status` and `axi logs`.

#### Scenario: Skill drift check stays green

- **WHEN** the skill drift check runs after the shortcuts are added
- **THEN** it passes, because the skill body still references `axi status` / `axi logs` and is not changed to mention `st` / `lg`
