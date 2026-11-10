//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package quotarouting

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReadSnapshotReapsDescendantsOnEveryExit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		startup string
	}{
		{name: "success", mode: "success"},
		{name: "error", mode: "error"},
		{name: "parent-cancellation", mode: "timeout"},
		{name: "slow-startup", mode: "success", startup: "sleep 0.3\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode := tc.mode
			const startupWindow = 5 * time.Second
			const operationWindow = 200 * time.Millisecond
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "child.pid")
			releaseFile := filepath.Join(dir, "release")
			if err := syscall.Mkfifo(releaseFile, 0o600); err != nil {
				t.Fatal(err)
			}
			// Holding both ends lets the fixture open its reader without
			// blocking setup, and buffers the release if it arrives first.
			release, err := os.OpenFile(releaseFile, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer release.Close()
			t.Setenv("NM_QUOTA_CHILD_PID", pidFile)
			t.Setenv("NM_QUOTA_RELEASE", releaseFile)
			script := "#!/bin/sh\n" + tc.startup + "sleep 30 &\nprintf '%s' \"$!\" > \"$NM_QUOTA_CHILD_PID\"\nread release < \"$NM_QUOTA_RELEASE\"\n"
			switch mode {
			case "success":
				script += "printf '%s' '{\"schemaVersion\":5,\"providers\":[]}'\n"
			case "error":
				script += "exit 3\n"
			case "timeout":
				script += "wait\n"
			}
			executable := filepath.Join(dir, "quota-axi")
			if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			// Cold shell startup can exceed 200ms under -race before the
			// first script instruction runs. Bound that setup separately so
			// every case actually exercises cleanup of a live descendant.
			ctx, cancel := context.WithCancel(context.Background())
			type response struct {
				snapshot *Snapshot
				err      error
				finished time.Time
			}
			results := make(chan response, 1)
			done := make(chan struct{})
			fixtureStart := time.Now()
			go func() {
				defer close(done)
				// The outer bound covers setup. The parent is canceled by
				// the 200ms operation deadline once the descendant is ready.
				snapshot, err := ReadSnapshot(ctx, executable, startupWindow+operationWindow)
				results <- response{snapshot: snapshot, err: err, finished: time.Now()}
			}()
			defer func() {
				cancel()
				<-done
			}()
			startup := time.NewTimer(startupWindow)
			defer startup.Stop()
			for {
				if data, err := os.ReadFile(pidFile); err == nil && len(data) > 0 {
					break
				}
				select {
				case result := <-results:
					t.Fatalf("fixture exited before descendant readiness: %v", result.err)
				case <-startup.C:
					t.Fatal("fixture did not create its descendant within the startup bound")
				case <-time.After(time.Millisecond):
				}
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			if pid <= 1 {
				t.Fatalf("invalid fixture descendant PID: %d", pid)
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("fixture descendant is not live before the operation: %v", err)
			}
			state, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
			if err != nil || strings.TrimSpace(string(state)) == "" || strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
				t.Fatalf("fixture descendant has not started live execution: state=%s err=%v", state, err)
			}
			operationStart := time.Now()
			operationCtx, stopOperation := context.WithTimeout(ctx, operationWindow)
			defer stopOperation()
			stopCancellation := context.AfterFunc(operationCtx, cancel)
			defer stopCancellation()
			if _, err := release.WriteString("run\n"); err != nil {
				t.Fatal(err)
			}
			result := <-results
			if (mode == "success") != (result.err == nil) {
				t.Fatalf("mode=%s snapshot=%+v error=%v", mode, result.snapshot, result.err)
			}
			elapsed := result.finished.Sub(operationStart)
			t.Logf("mode=%s startup=%s operation=%s", mode, operationStart.Sub(fixtureStart), elapsed)
			if mode == "timeout" {
				if !errors.Is(operationCtx.Err(), context.DeadlineExceeded) || ctx.Err() != context.Canceled || elapsed < operationWindow || elapsed >= operationWindow+2*time.Second {
					t.Fatalf("fixture did not hit the 200ms operation deadline: elapsed=%s err=%v", elapsed, operationCtx.Err())
				}
			} else if elapsed >= operationWindow {
				t.Fatalf("mode=%s exceeded the 200ms operation deadline: %s", mode, elapsed)
			}
			if mode == "error" && !strings.Contains(result.err.Error(), "exit status 3") {
				t.Fatalf("fixture error was replaced: %v", result.err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
					break
				}
				state, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
				if err == nil && strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("mode=%s descendant %d remains alive: %s", mode, pid, state)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// The reader's configured deadline must bound execution independently of parent
// cancellation. The readiness-gated cases above separately prove descendant
// cleanup even when shell startup itself takes longer than the operation budget.
// Replacing ReadSnapshot's WithTimeout with WithCancel must fail this case at
// the outer guard while leaving the parent-cancellation case passing.
func TestReadSnapshotOwnTimeout(t *testing.T) {
	const operationWindow = 200 * time.Millisecond
	dir := t.TempDir()
	releaseFile := filepath.Join(dir, "release")
	if err := syscall.Mkfifo(releaseFile, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := os.OpenFile(releaseFile, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release.Close()
	t.Setenv("NM_QUOTA_RELEASE", releaseFile)
	executable := filepath.Join(dir, "quota-axi")
	// A shell builtin blocks without spawning a child or imposing another
	// deadline that could accidentally substitute for the reader's timeout.
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nread release < \"$NM_QUOTA_RELEASE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 1)
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		_, err := ReadSnapshot(ctx, executable, operationWindow)
		results <- err
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case err := <-results:
		elapsed := time.Since(start)
		if ctx.Err() != nil || elapsed < operationWindow || elapsed >= operationWindow+2*time.Second || err == nil || !strings.Contains(err.Error(), "did not finish within 200ms") {
			t.Fatalf("reader did not enforce its own deadline: elapsed=%s parent=%v error=%v", elapsed, ctx.Err(), err)
		}
		t.Logf("reader timeout: elapsed=%s parent=%v error=%v", elapsed, ctx.Err(), err)
	case <-time.After(5 * time.Second):
		// This guard only fails the test. Deferred cancellation then cleans
		// up; parent cancellation can never produce a passing result.
		t.Fatal("reader did not return within the outer failure guard")
	}
}

func TestReadSnapshotRequestsClaudeProfileSelectorMetadata(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "quota-axi")
	script := `#!/bin/sh
if [ "$*" != '--full --json' ]; then exit 3; fi
printf '%s' '{"schemaVersion":6,"providers":[{"provider":"claude","accountKey":"profile:selected","accountKeys":["profile:selected"],"accountLocator":{"kind":"config-dir","path":"/selected-profile","delegateEligible":true},"quotaSemantics":{"status":"known","effectiveAvailability":[{"scope":"all_models","status":"known","effectivePercentRemaining":0}]}}]}'
`
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadSnapshot(context.Background(), executable, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	selection := Select(snapshot, []Candidate{{Agent: types.AgentClaude}}, 90*time.Minute)
	evidence := selection.EvidenceFor(types.AgentClaude)
	if evidence.Eligible || evidence.Account != "profile:selected" || evidence.Remaining == nil || *evidence.Remaining != 0 {
		t.Fatalf("selected Claude profile exhaustion was discarded: %+v", evidence)
	}
}
