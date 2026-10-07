# Retiring a workstation's hand-built trial of no-mistakes (2026-10-07)

Fleet-side operational record. No product, code or documentation change: this file only records
a workstation swapping a locally built trial of the explicit-declines fix for upstream's own
build, so the machine runs what upstream ships.

## Before: the trial build

| | |
|---|---|
| path | `~/.no-mistakes/bin/no-mistakes`, also reached as `~/.local/bin/no-mistakes` |
| reported version | `v1.88.1-0.20261005182235-d9820b647469 (unknown) unknown` |
| built from | `d9820b647469`, the branch head of the explicit finding declines work before it was merged as `fa8c12c` (pull request 1351) |
| built at | 2026-10-05T20:00:45Z |
| sha256 | `9ba1fbcd6369757218ea9ffcf30f30e1d5f628002f70721722f8cfe632b3647d` |
| daemon | systemd user unit `no-mistakes-daemon-4635761c.service`, started 2026-10-05T20:00:45Z |

## After: upstream's build

| | |
|---|---|
| source | `kunchenguid/no-mistakes` default branch, commit `391975ce329b3730a7a7dba08eb2d853d7cd470f` (tag `v1.90.0`), the tip that contains `fa8c12c` |
| toolchain | go1.27.1, `make build` (Makefile `VERSION`/`COMMIT`/`DATE` stamping) |
| reported version | `v1.90.0 (391975c) 2026-10-07T15:47:51Z` |
| sha256 | `63b69cfdc6c77ce7563c22c223fa27ff68c6398684d186d682ff4d18fc82cc3e` |
| installed by | renaming the new file over the live path, so nothing is ever written through a running executable |
| rollback copy | `~/.no-mistakes-backup-2026-10-07-v1.88.1-d9820b6/no-mistakes`, with the details beside it in `VERSION.txt` |

## The live switch, and rollback

The daemon keeps the binary it started with, so installing the new build does not disturb a
running one and does not by itself change which build serves runs. The new build becomes the
live one on the tool's own restart, which is the supported switch:

```sh
no-mistakes daemon restart
```

`daemon restart` refuses while pipeline runs are active; run it between runs rather than
passing `--force`, which would kill runs that are mid-validation. Rollback, one command, is the
same rename discipline for the same reason:

```sh
cp ~/.no-mistakes-backup-2026-10-07-v1.88.1-d9820b6/no-mistakes ~/.no-mistakes/bin/.no-mistakes.restore \
  && mv -f ~/.no-mistakes/bin/.no-mistakes.restore ~/.no-mistakes/bin/no-mistakes \
  && no-mistakes daemon restart
```

## Verification performed

- the installed binary reports `v1.90.0 (391975c) 2026-10-07T15:47:51Z`
- `no-mistakes doctor` passes with it, and `no-mistakes status` answers over IPC against the
  running daemon
- `no-mistakes daemon status` reports the daemon running, unchanged pid across the swap
- the running daemon's executable is the replaced trial file (the swap renames, so the daemon's
  inode survives as `... (deleted)` until the restart above)
