package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// agent_run_pi_test.go is process-level coverage of the pi liveness fix: a
// real piAgent adapter drives a fake pi subprocess whose stdout stays quiet
// (pi buffers it when piped) while its exact session JSONL keeps advancing.
// The fake redirects session storage through PI_CODING_AGENT_DIR so neither
// the adapter's watcher nor the fixture ever touches the operator's real
// ~/.pi. The budgets are sized above the watcher's 2s poll interval so the
// tests exercise the production polling path without test hooks.

const piLivenessTestSessionID = "019ff2f3-5f31-744b-90b8-679074ff7686"

// fakePiSessionScript appends one session event every 250ms for the given
// count while printing nothing, then emits the JSON-mode session header and
// agent_end. With count=0 it writes the header event to the session file and
// then freezes forever (until the watchdog kills the process tree).
func fakePiSessionScript(appendCount int) string {
	return `#!/bin/sh
cat > /dev/null
# pi encodes process.cwd(), the physical path: pwd -P matches it.
enc=$(pwd -P | sed -e 's|^/||' -e 's|/|-|g')
session_dir="$PI_CODING_AGENT_DIR/sessions/--${enc}--"
mkdir -p "$session_dir"
session_file="$session_dir/2026-08-26T00-00-00-000Z_` + piLivenessTestSessionID + `.jsonl"
printf '%s\n' '{"type":"session","id":"` + piLivenessTestSessionID + `","timestamp":"2026-08-26T00:00:00Z","cwd":"'"$(pwd)"'"}' >> "$session_file"
i=0
while [ "$i" -lt ` + strconv.Itoa(appendCount) + ` ]; do
  printf '%s\n' '{"type":"message","message":{"role":"assistant","content":"working"}}' >> "$session_file"
  sleep 0.25
  i=$((i+1))
done
if [ ` + strconv.Itoa(appendCount) + ` -eq 0 ]; then
  while true; do sleep 1; done
fi
printf '%s\n' '{"type":"session","id":"` + piLivenessTestSessionID + `"}'
printf '%s\n' '{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"done"}]}]}'
`
}

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func writeFakePiScript(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	return bin
}

func newPiStepContext(t *testing.T, bin, agentDir string, budget time.Duration) *StepContext {
	t.Helper()
	ag, err := agent.NewWithOptions(types.AgentPi, bin, nil, agent.Options{})
	if err != nil {
		t.Fatalf("new pi agent: %v", err)
	}
	return &StepContext{
		Ctx:    context.Background(),
		Agent:  ag,
		Config: &config.Config{AgentTimeout: budget},
		Env:    []string{"PI_CODING_AGENT_DIR=" + agentDir},
	}
}

// The incident class from run 01M0WRW741G2YPQFXYE5TG6DZD: the fixer's stdout
// was buffered into silence while its pi session stayed healthy to the end.
// The invocation must survive far past the configured silence budget on
// session-event evidence alone.
func TestRunAgent_PiSessionActivityKeepsBufferedStdoutInvocationAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	agentDir := t.TempDir()
	workDir := t.TempDir()
	bin := writeFakePiScript(t, fakePiSessionScript(26)) // ~6.5s of session activity

	sctx := newPiStepContext(t, bin, agentDir, 3*time.Second)
	opts := agent.RunOpts{
		Prompt:  "work",
		CWD:     workDir,
		Env:     sctx.Env,
		Session: &agent.SessionRef{}, // durable: pi persists a session JSONL
	}
	start := time.Now()
	result, err := sctx.RunAgent(opts)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("invocation with an advancing session must survive a quiet stdout, got %v (after %s)", err, elapsed)
	}
	if result == nil || result.SessionID != piLivenessTestSessionID {
		t.Fatalf("result = %+v, want session %s", result, piLivenessTestSessionID)
	}
	if elapsed < 6*time.Second {
		t.Fatalf("invocation ended after %s; the fake agent was active for ~6.5s, so the run was cut short", elapsed)
	}
	if !strings.Contains(result.Text, "done") {
		t.Fatalf("result text = %q, want the agent's final output", result.Text)
	}
}

// The guard that must never weaken: when neither stdout nor the bound session
// makes progress for the full budget, the invocation is terminated with its
// process tree and the diagnostic names the evidence.
func TestRunAgent_FrozenPiSessionIsKilledAfterTheFullBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	agentDir := t.TempDir()
	workDir := t.TempDir()
	bin := writeFakePiScript(t, fakePiSessionScript(0)) // session header, then frozen

	sctx := newPiStepContext(t, bin, agentDir, 3*time.Second)
	opts := agent.RunOpts{
		Prompt:  "work",
		CWD:     workDir,
		Env:     sctx.Env,
		Session: &agent.SessionRef{},
	}
	start := time.Now()
	_, err := sctx.RunAgent(opts)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a frozen agent with no stdout and no session advancement must be killed")
	}
	if !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	var ate *AgentTimeoutError
	if !errors.As(err, &ate) {
		t.Fatalf("error = %v, want AgentTimeoutError evidence", err)
	}
	if !strings.Contains(ate.Evidence, "pi session events") {
		t.Fatalf("evidence = %q, want the bound session's last activity named", ate.Evidence)
	}
	// Killed by its own budget, not instantly and not never: last session
	// event lands within one poll interval of launch, then the full budget
	// of silence must elapse.
	if elapsed < 3*time.Second {
		t.Fatalf("killed after %s, before the full 3s silence budget elapsed", elapsed)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("kill took %s; the watchdog must bound a frozen invocation", elapsed)
	}
}

// A session-free pi invocation (--no-session) has no session file to bind:
// only stdout and lifecycle evidence govern it. With a quiet stdout it is
// genuinely silent and must be killed at the budget.
func TestRunAgent_SessionFreePiQuietStdoutIsKilledAtBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	agentDir := t.TempDir()
	workDir := t.TempDir()
	bin := writeFakePiScript(t, `#!/bin/sh
cat > /dev/null
while true; do sleep 1; done
`)

	sctx := newPiStepContext(t, bin, agentDir, 2*time.Second)
	// No durable session: buildArgs adds --no-session, pi persists nothing.
	opts := agent.RunOpts{Prompt: "work", CWD: workDir, Env: sctx.Env}
	start := time.Now()
	_, err := sctx.RunAgent(opts)
	elapsed := time.Since(start)
	if err == nil || !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want ErrAgentTimeout", err)
	}
	var ate *AgentTimeoutError
	if errors.As(err, &ate) {
		if strings.Contains(ate.Evidence, "pi session events") {
			t.Fatalf("evidence = %q; a --no-session invocation must never credit session activity", ate.Evidence)
		}
		if !strings.Contains(ate.Evidence, "process lifecycle") {
			t.Fatalf("evidence = %q, want only lifecycle (process start) evidence", ate.Evidence)
		}
	}
	if elapsed < 2*time.Second || elapsed > 20*time.Second {
		t.Fatalf("killed after %s, want ~2s silence budget", elapsed)
	}
}
