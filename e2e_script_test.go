package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestE2EScriptDefaultBudgetAndCallerOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("e2e wrapper is POSIX-oriented")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	fakeBin := t.TempDir()
	fakeGo := filepath.Join(fakeBin, "go")
	fakeGoScript := `#!/bin/sh
printf '%s\n' "$*" >> "$E2E_GO_LOG"
for arg in "$@"; do
  if [ "$arg" = "-list" ]; then
    printf '%s\n' TestAlpha TestUserJourney TestBeta TestGamma TestDelta
    break
  fi
done
`
	if err := os.WriteFile(fakeGo, []byte(fakeGoScript), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, args ...string) []string {
		t.Helper()
		inventory := t.TempDir()
		logPath := filepath.Join(t.TempDir(), "go-calls.log")
		argv := append([]string{"scripts/e2e.sh"}, args...)
		cmd := exec.Command(bash, argv...)
		cmd.Env = filteredEnv(os.Environ(), "PATH", "E2E_GO_LOG", "NM_E2E_DAEMON_INVENTORY", "NM_E2E_DAEMON_INVENTORY_PARENT", "NM_E2E_REAP_ABANDONED")
		cmd.Env = append(cmd.Env,
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"E2E_GO_LOG="+logPath,
			"NM_E2E_DAEMON_INVENTORY="+inventory,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("e2e wrapper: %v\n%s", err, out)
		}
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}

	t.Run("canonical default discovers and shards every test", func(t *testing.T) {
		calls := run(t)
		if len(calls) != 7 {
			t.Fatalf("go calls = %q, want discovery, four e2e shards, step packages, and cleanup", calls)
		}
		if !strings.Contains(calls[0], "test -tags=e2e -count=1 -timeout 60s -list ^Test ./internal/e2e") {
			t.Fatalf("discovery call = %q", calls[0])
		}
		joinedRuns := strings.Join(calls[1:len(calls)-1], "\n")
		for _, testName := range []string{"TestAlpha", "TestUserJourney", "TestBeta", "TestGamma", "TestDelta"} {
			if got := strings.Count(joinedRuns, testName); got != 1 {
				t.Fatalf("%s appears in %d execution shards, want exactly one\n%s", testName, got, joinedRuns)
			}
		}
		for _, call := range calls[1 : len(calls)-2] {
			if !strings.Contains(call, "-timeout 600s") || !strings.HasSuffix(call, "./internal/e2e") {
				t.Fatalf("internal/e2e shard lacks its bounded package timeout: %q", call)
			}
		}
		if got := calls[len(calls)-2]; got != "test -tags=e2e -count=1 -timeout 600s ./internal/pipeline/steps/..." {
			t.Fatalf("step-local e2e call = %q", got)
		}
		if got := calls[len(calls)-1]; got != "run ./internal/e2edaemon/reapmain.go" {
			t.Fatalf("cleanup call = %q, want inventory reaper", got)
		}
	})

	t.Run("caller override remains one exact command", func(t *testing.T) {
		calls := run(t, "-tags=e2e", "-count=1", "-timeout", "42s", "./internal/e2e", "-run", "^TestUserJourney/antigravity$")
		if len(calls) != 2 {
			t.Fatalf("go calls = %q, want caller command plus cleanup reaper", calls)
		}
		want := "test -tags=e2e -count=1 -timeout 42s ./internal/e2e -run ^TestUserJourney/antigravity$"
		if calls[0] != want {
			t.Fatalf("test call = %q, want %q", calls[0], want)
		}
		if calls[1] != "run ./internal/e2edaemon/reapmain.go" {
			t.Fatalf("cleanup call = %q, want inventory reaper", calls[1])
		}
	})
}
