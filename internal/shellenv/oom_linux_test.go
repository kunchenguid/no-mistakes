//go:build linux

package shellenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/shellenv/memscopetest"
)

func TestStartShellCommandRaisesChildOOMScore(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	ConfigureShellCommand(cmd)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { TerminateShellCommandGroup(cmd) })

	got, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/oom_score_adj")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != stepOOMScoreAdj {
		t.Fatalf("oom_score_adj = %q, want %s", strings.TrimSpace(string(got)), stepOOMScoreAdj)
	}
}

func TestOwnOOMScoreScriptRaisesBeforeThePayload(t *testing.T) {
	cmd := exec.Command("sh", "-c", OwnOOMScoreScript("cat /proc/self/oom_score_adj"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != stepOOMScoreAdj {
		t.Fatalf("score = %q, want %s", strings.TrimSpace(string(out)), stepOOMScoreAdj)
	}
}

func TestOwnOOMScoreScriptUnwritableScoreAddsNoOutput(t *testing.T) {
	unopenable := filepath.Join(t.TempDir(), "missing", "oom_score_adj")
	out, err := exec.Command("sh", "-c", ownOOMScoreScript(unopenable, "echo payload")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if string(out) != "payload\n" {
		t.Fatalf("output = %q, want only the payload", out)
	}
}

func TestAttributeOOMKillLeavesAPlainFailureWithoutACounterRise(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 137").Run()
	if AttributeOOMKill(^uint64(0), true, err) != err {
		t.Fatal("exit 137 with no counter rise must stay a plain failure")
	}
	if AttributeOOMKill(0, false, err) != err {
		t.Fatal("missing cgroup baseline must stay a plain failure")
	}
}

// TestOOMKillAttributesTheAllocatingCommandAndSparesItsNeighbor runs under a
// scope with OOMPolicy=continue. The allocating command is attributed and the
// neighbor command, plus this process, keep running.
func TestOOMKillAttributesTheAllocatingCommandAndSparesItsNeighbor(t *testing.T) {
	if os.Getenv("NM_OOM_INNER") == "1" {
		runOOMNeighborInner(t)
		return
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip(err)
	}
	memscopetest.RequireDelegatedMemoryLimit(t, oomScopeMemoryMiB)
	cmd := exec.Command("systemd-run", "--user", "--scope",
		"-p", "MemoryMax="+strconv.Itoa(oomScopeMemoryMiB)+"M",
		"-p", "MemorySwapMax=0",
		"-p", "OOMPolicy=continue",
		"--",
		os.Args[0], "-test.run", "^TestOOMKillAttributesTheAllocatingCommandAndSparesItsNeighbor$", "-test.count=1")
	cmd.Env = append(os.Environ(), "NM_OOM_INNER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scoped run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "neighbor-still-alive") {
		t.Fatalf("neighbor did not survive:\n%s", out)
	}
}

const oomScopeMemoryMiB = 160

func runOOMNeighborInner(t *testing.T) {
	t.Helper()
	neighbor := exec.CommandContext(context.Background(), "sh", "-c", "echo neighbor-started; sleep 30")
	ConfigureShellCommand(neighbor)
	if err := StartShellCommand(neighbor); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { TerminateShellCommandGroup(neighbor) })

	bomb := exec.CommandContext(context.Background(), "sh", "-c", OwnOOMScoreScript(
		"python3 -c 'b=bytearray(400*1024*1024)'"))
	ConfigureShellCommand(bomb)
	err := RunShellCommand(bomb)
	if !errors.Is(err, ErrOutOfMemory) {
		t.Fatalf("allocating command error = %v, want ran out of memory", err)
	}
	if err := syscall.Kill(neighbor.Process.Pid, 0); err != nil {
		t.Fatalf("neighbor pid %d not alive: %v", neighbor.Process.Pid, err)
	}
	fmt.Println("neighbor-still-alive")
}
