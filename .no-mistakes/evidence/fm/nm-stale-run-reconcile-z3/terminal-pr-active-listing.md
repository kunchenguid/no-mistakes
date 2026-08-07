# Terminal PR lifecycle CLI evidence

The isolated end-to-end regression created an authoritative `running` run,
applied the terminal PR observation, and then invoked the real
`no-mistakes runs` CLI against its isolated daemon and home.

Command:

```console
$ ./scripts/e2e.sh -tags=e2e -count=1 -timeout 120s ./internal/e2e -run '^TestTerminalPRRunDisappearsFromActiveListing$' -v
```

Merged PR observation:

```console
$ no-mistakes runs
  completed    feature/terminal-pr  01234567  2026-07-23 23:58  https://github.com/test/repo/pull/42
```

Closed PR observation:

```console
$ no-mistakes runs
  completed    feature/terminal-pr  01234567  2026-07-23 23:58  https://github.com/test/repo/pull/42
```

Both E2E subtests passed, and their direct database check found zero active
runs after the CLI observation.
