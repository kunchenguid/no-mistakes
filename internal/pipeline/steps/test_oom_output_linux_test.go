//go:build linux

package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/shellenv/memscopetest"
)

// TestTestStep_OutOfMemoryKeepsTheCommandOutputInTheStepLog runs under a
// memory-limited scope with OOMPolicy=continue so the configured test command
// is OOM-killed after printing a marker.
func TestTestStep_OutOfMemoryKeepsTheCommandOutputInTheStepLog(t *testing.T) {
	if os.Getenv("NM_STEPS_OOM_INNER") == "1" {
		runTestStepOOMInner(t)
		return
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip(err)
	}
	memscopetest.RequireDelegatedMemoryLimit(t, testStepOOMScopeMiB)
	cmd := exec.Command("systemd-run", "--user", "--scope", "--quiet",
		"-p", "MemoryMax="+strconv.Itoa(testStepOOMScopeMiB)+"M",
		"-p", "MemorySwapMax=0",
		"-p", "OOMPolicy=continue",
		"--",
		os.Args[0], "-test.run", "^TestTestStep_OutOfMemoryKeepsTheCommandOutputInTheStepLog$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "NM_STEPS_OOM_INNER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scoped run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "oom-output-logged") {
		t.Fatalf("inner test did not confirm the logged output:\n%s", out)
	}
}

const testStepOOMScopeMiB = 256

func runTestStepOOMInner(t *testing.T) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("agent must not run after the configured command ran out of memory")
		return nil, nil
	}}
	testCmd := "echo marker-before-allocation; python3 -c 'b=bytearray(1024*1024*1024)'"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{Test: testCmd})
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }

	_, err := (&TestStep{}).Execute(sctx)
	if !errors.Is(err, shellenv.ErrOutOfMemory) {
		t.Fatalf("Execute() error = %v, want ran out of memory", err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "marker-before-allocation") {
		t.Fatalf("step log lost the command output: %q", logs)
	}
	fmt.Println("oom-output-logged")
}
