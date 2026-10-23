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
	for _, mode := range []string{"success", "error", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "child.pid")
			t.Setenv("NM_QUOTA_CHILD_PID", pidFile)
			script := "#!/bin/sh\nsleep 30 &\nprintf '%s' \"$!\" > \"$NM_QUOTA_CHILD_PID\"\n"
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
			snapshot, err := ReadSnapshot(context.Background(), executable, 200*time.Millisecond)
			if (mode == "success") != (err == nil) {
				t.Fatalf("mode=%s snapshot=%+v error=%v", mode, snapshot, err)
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
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
