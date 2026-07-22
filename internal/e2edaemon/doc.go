// Package e2edaemon owns temporary E2E daemon lifecycle: exact inventory,
// a reaper with bounded ownership checks, and a concurrency slot cap.
//
// Scope is temporary E2E daemons only (NM_TEST_START_DAEMON detached starts
// under harness temp roots). The shared production daemon and external
// sleep-loop keepalive shells are out of scope.
//
// Recovery boundary (honest):
//   - t.Cleanup and package TestMain reapers cover normal completion and
//     most interrupt paths that leave the Go process able to run cleanups.
//   - scripts/e2e.sh traps EXIT/INT/TERM on the suite wrapper shell so a
//     killed or timed-out go-test child still gets inventory reaped.
//   - A SIGKILL of that same wrapper shell does NOT run its EXIT trap.
//     Stale inventory on disk is recovered on the next suite start
//     (TestMain + wrapper pre-reap). Do not claim shell traps survive
//     SIGKILL of the trapping shell.
package e2edaemon
