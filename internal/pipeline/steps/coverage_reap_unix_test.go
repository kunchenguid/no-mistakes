//go:build unix

package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// These regression tests pin the process-tree reap wiring on each coverage
// provider's subprocess. A coverage command (go test, c8/npx, ssh) that shells
// out can leave a worker pool alive after the leader exits 0 - exactly the leak
// path that, across runs, OOM-kills the daemon (see AGENTS.md §Context,
// Concurrency, and Processes). Each provider must route its *exec.Cmd through
// shellenv.ConfigureShellCommand + a reap helper (CombinedOutputShellCommand /
// OutputShellCommand) so the whole process group is reaped on a clean exit too,
// not only on context cancellation.
//
// The providers hardcode their command (go/npx/ssh), so each test installs a
// fake of that binary ahead of PATH. The fake backgrounds a long-lived
// grandchild with detached stdio (so it does not hold the leader's pipes open
// and instead exercises the clean-exit leak path), records its pid, writes the
// output the provider expects to read back, and exits 0. After the provider
// returns, the grandchild must be gone - proving the group was reaped. Drop the
// ConfigureShellCommand/reap-helper wiring and the grandchild survives.

// writeFakeCoverageBin writes an executable #!/bin/sh script named name into
// dir and returns dir, so it can be put ahead of PATH.
func writeFakeCoverageBin(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// pidGoneWithin reports whether pid stops existing within window. kill(pid, 0)
// returns ESRCH once the process is gone (the grandchild reparents to init
// after the leader exits, so init reaps it the moment it is SIGKILLed).
func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

// TestGoCoverageProfile_ReapsGrandchildOnCleanExit is the clean-exit regression
// for the Go coverage provider. `go test -cover ./...` compiles and runs the
// test binary, which can spawn worker processes that outlive it. Without the
// reap wiring the grandchild leaks on a green test run; with it the whole
// process group is reaped the moment the provider returns.
func TestGoCoverageProfile_ReapsGrandchildOnCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	t.Setenv("NM_REAP_PIDFILE", pidFile)

	binDir := fakeCLIBinDir(t)
	writeFakeCoverageBin(t, binDir, "go", `( sleep 120 >/dev/null 2>&1 ) &
echo $! > "$NM_REAP_PIDFILE"
prof=""
for a in "$@"; do
	case "$a" in -coverprofile=*) prof="${a#-coverprofile=}";; esac
done
[ -n "$prof" ] && printf 'mode: set\n' > "$prof"
exit 0`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: dir,
		Config:  &config.Config{},
		Log:     func(string) {},
	}
	if _, _, err := runGoCoverageProfile(sctx); err != nil {
		t.Fatalf("runGoCoverageProfile returned error (reap must not break the happy path): %v", err)
	}

	grandchild := waitForIntFile(t, pidFile, 5*time.Second)
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d still alive after clean go test exit; the process group leaked "+
			"(this is the leak that OOM-kills the daemon)", grandchild)
	}
}

// TestGoCoverageProfile_KillsGrandchildOnCancel is the cancellation
// counterpart: ConfigureShellCommand installs a cmd.Cancel that SIGKILLs the
// whole group when the step's context is cancelled, so a leaking worker the
// test binary spawned is torn down with the leader, not left running and
// holding the worktree locked.
func TestGoCoverageProfile_KillsGrandchildOnCancel(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	hb := filepath.Join(dir, "tick")
	t.Setenv("NM_REAP_PIDFILE", pidFile)
	t.Setenv("NM_REAP_HB", hb)

	binDir := fakeCLIBinDir(t)
	writeFakeCoverageBin(t, binDir, "go", `( i=0; while [ $i -lt 10000 ]; do printf '%s\n' "$i" > "$NM_REAP_HB"; sleep 0.1; i=$((i+1)); done ) >/dev/null 2>&1 &
echo $! > "$NM_REAP_PIDFILE"
wait`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sctx := &pipeline.StepContext{Ctx: ctx, WorkDir: dir, Config: &config.Config{}, Log: func(string) {}}

	done := make(chan error, 1)
	go func() {
		_, _, err := runGoCoverageProfile(sctx)
		done <- err
	}()

	grandchild := waitForIntFile(t, pidFile, 5*time.Second)
	waitForHeartbeatChange(t, hb, 3*time.Second) // prove the grandchild is actually running before we cancel
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatal("runGoCoverageProfile did not return within 5s of cancel")
	}
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d still alive after cancel; the process group was not killed", grandchild)
	}
}

// TestJSCoverage_RunCoverage_ReapsGrandchildOnCleanExit is the clean-exit
// regression for the JS/TS coverage provider. c8 wraps the project's test
// runner (`npm test`, `node --test`, ...) whose worker pool is the canonical
// grandchild that outlives the leader on a green run. The provider runs npx
// under CombinedOutputShellCommand so the group is reaped on success too.
func TestJSCoverage_RunCoverage_ReapsGrandchildOnCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	t.Setenv("NM_REAP_PIDFILE", pidFile)

	binDir := fakeCLIBinDir(t)
	writeFakeCoverageBin(t, binDir, "npx", `( sleep 120 >/dev/null 2>&1 ) &
echo $! > "$NM_REAP_PIDFILE"
rep=""
for a in "$@"; do
	case "$a" in --reports-dir=*) rep="${a#--reports-dir=}";; esac
done
[ -n "$rep" ] && printf '{}\n' > "$rep/coverage-final.json"
exit 0`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: dir,
		Config:  &config.Config{},
		Log:     func(string) {},
	}
	if _, _, err := (jsCoverageProvider{}).RunCoverage(sctx); err != nil {
		t.Fatalf("js RunCoverage returned error (reap must not break the happy path): %v", err)
	}

	grandchild := waitForIntFile(t, pidFile, 5*time.Second)
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d still alive after clean c8/npx exit; the process group leaked", grandchild)
	}
}

// TestSwiftCoverageSSH_ReapsGrandchildOnCleanExit is the clean-exit regression
// for the Swift coverage SSH path. runSwiftCoverageSSH keeps stderr separate
// from the JSON stream by capturing it into its own buffer, then routes the
// command through OutputShellCommand (not cmd.Output) so the reap fires on a
// clean exit too. A helper the ssh client spawned (or a ControlMaster it left
// behind) must not outlive the leader.
func TestSwiftCoverageSSH_ReapsGrandchildOnCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	t.Setenv("NM_REAP_PIDFILE", pidFile)

	binDir := fakeCLIBinDir(t)
	writeFakeCoverageBin(t, binDir, "ssh", `cat >/dev/null
( sleep 120 >/dev/null 2>&1 ) &
echo $! > "$NM_REAP_PIDFILE"
printf '{}\n'
exit 0`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := runSwiftCoverageSSH(context.Background(), "fakehost", nil, "set -e\necho build\n")
	if err != nil {
		t.Fatalf("runSwiftCoverageSSH returned error (reap must not break the happy path): %v", err)
	}
	if strings.TrimSpace(out) != "{}" {
		t.Fatalf("swift ssh stdout = %q, want {}", out)
	}

	grandchild := waitForIntFile(t, pidFile, 5*time.Second)
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d still alive after clean ssh exit; the process group leaked", grandchild)
	}
}
