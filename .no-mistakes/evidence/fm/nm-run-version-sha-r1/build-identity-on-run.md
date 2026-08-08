# Build identity recorded on SQLite run records

Intent: every newly created and subsequently updated run record must retain durable
version and build/git SHA fields, using the same identity users see via `--version`,
via a safe additive nullable migration, proven behaviorally through the real SQLite
write/read path.

The binary and the DB write path were built with the same release-style `-ldflags`
(`buildinfo.Version=v1.47.0`, `buildinfo.Commit=df002d9`) so the two surfaces are
directly comparable.

## 1) Identity a user sees via `--version`

```
$ no-mistakes --version
no-mistakes version v1.47.0 (df002d9) 2026-08-07
```

## 2) Identity persisted on a real run row (InsertRun -> UpdateRunStatus -> raw SELECT)

Exercised the real `db.InsertRun` + `db.UpdateRunStatus` path, then read the RAW
persisted SQLite columns back directly (not through the scan helper):

```
binary --version identity : buildinfo.String() = "v1.47.0 (df002d9) 2026-08-07"
  CurrentVersion()="v1.47.0"  Commit="df002d9"
persisted runs row        : id=01KZFY870743W0BAA9KRAAQ6EG status=running
  runs.no_mistakes_version   = v1.47.0
  runs.no_mistakes_build_sha = df002d9
MATCH: persisted DB identity == running binary --version identity
```

The version/SHA land on the run at insert and survive a status update; they are the
exact identity `--version` reports.

## 3) Backward-compatible additive nullable migration

`TestOpenMigratesRunSyncProvenanceWithoutBackfillingMutableHead` confirms a legacy
run row (predating these columns) reads back with `no_mistakes_version == NULL` and
`no_mistakes_build_sha == NULL` - never backfilled from mutable head - while
`TestOpenCreatesSchema` confirms both columns exist on a fresh schema.

## Targeted tests run (all pass)

```
go test -race -run 'TestRunInsertAndUpdatePreserveBuildIdentity|TestOpenMigratesRunSyncProvenanceWithoutBackfillingMutableHead|TestOpenCreatesSchema|TestInsertRunWithIntent|TestRunInsertAndGet' ./internal/db/
--- PASS: TestOpenCreatesSchema
--- PASS: TestOpenMigratesRunSyncProvenanceWithoutBackfillingMutableHead
--- PASS: TestRunInsertAndGet
--- PASS: TestRunInsertAndUpdatePreserveBuildIdentity
--- PASS: TestInsertRunWithIntent
ok  github.com/kunchenguid/no-mistakes/internal/db
```

No secrets are stored: only the public version string and the short build SHA are
recorded, the same values printed by `--version`.
