# TUI Custody Recovery Render

This artifact captures the terminal UI strings for a terminal pre-push `pipeline_owned` run.

## Local Branch Status

```text
╭─ Local branch ───────────────────────────────────────────────────────────────────────────────────╮
│ Run ended without publishing its pipeline commits; they are preserved in the local gate. Recover custody to take the branch back, or rerun to resume validation. │
╰──── u recover custody ───────────────────────────────────────────────────────────────────────────╯
```

## Recovery Confirmation After Pressing `u`

```text
╭─ Pipeline ───────────────────────────────────────────────────────────────────────────────────────╮
│ feature/recover-evidence                                                               cancelled │
│                                                                                                  │
╰──────────────────────────────────────────────────────────────────────────────────────────────────╯

✗ Pipeline cancelled

╭─ Local branch ───────────────────────────────────────────────────────────────────────────────────╮
│ Run ended without publishing its pipeline commits; they are preserved in the local gate. Recover custody to take the branch back, or rerun to resume validation. │
╰──── u recover custody ───────────────────────────────────────────────────────────────────────────╯

╭─ Confirm custody recovery ───────────────────────────────────────────────────────────────────────╮
│ The run ended cancelled without publishing its pipeline commits. Recovery returns                │
│ custody of this branch and fast-forwards only a clean behind worktree.                           │
│                                                                                                  │
│ Local branch:   feature/recover-evidence                                                         │
│ Local HEAD:     aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa                                         │
│ Preserved HEAD: cccccccccccccccccccccccccccccccccccccccc                                         │
│                                                                                                  │
│ Dirty or diverged worktrees refuse without changes; `no-mistakes sync --recover                  │
│ --keep-local` keeps the current head instead. `no-mistakes rerun` resumes validation.            │
╰──── u/enter recover  ·  esc cancel ──────────────────────────────────────────────────────────────╯

  q quit  ? help  y yolo  r rerun
```
