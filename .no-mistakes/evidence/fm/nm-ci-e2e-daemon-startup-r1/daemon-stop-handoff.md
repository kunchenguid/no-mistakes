# Daemon stop and restart handoff

This focused E2E run exercised the built `no-mistakes` CLI through isolated
end-user `daemon stop`, `daemon restart`, and `daemon status` commands. Process
state was probed immediately when each command returned, with no grace period.

Command:

```console
./scripts/e2e.sh -tags=e2e -count=1 -timeout 120s ./internal/e2e \
  -run 'TestDaemon(StopLeavesNoDaemonProcessOwningTheRoot|RestartReplacesTheDaemonWithExactlyOneOwner)$' -v
```

Observed behavior:

```text
=== RUN   TestDaemonStopLeavesNoDaemonProcessOwningTheRoot
    daemon_stop_handoff_test.go:48: daemon stop output: ✓ daemon stopped; pid 86066 immediately exited; root owners: none
--- PASS: TestDaemonStopLeavesNoDaemonProcessOwningTheRoot (1.81s)
=== RUN   TestDaemonRestartReplacesTheDaemonWithExactlyOneOwner
    daemon_stop_handoff_test.go:88: daemon restart output: ✓ daemon restarted; previous pid 86147 immediately exited; replacement pid 86173 is the sole root owner; status: ● daemon running (pid 86173)
--- PASS: TestDaemonRestartReplacesTheDaemonWithExactlyOneOwner (1.09s)
PASS
ok  	github.com/kunchenguid/no-mistakes/internal/e2e	3.333s
```

The first command returned only after its daemon process had exited and no
process owned the isolated root. The restart command returned with its previous
process gone, exactly one replacement owner, and the user-facing status
reporting that replacement as running.
