package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/supervision"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCanonicalSupervisorCWD(t *testing.T) {
	got, err := canonicalSupervisorCWD(".")
	if err != nil {
		t.Fatalf("canonicalSupervisorCWD() error = %v", err)
	}
	if got == "." || !strings.HasPrefix(got, "/") {
		t.Fatalf("canonicalSupervisorCWD() = %q, want absolute clean path", got)
	}
}

func TestAxiSuperviseHelpNamesBothSupportedNativeAgents(t *testing.T) {
	for name, content := range map[string]string{
		"supervise": newAxiSuperviseCmd().Short,
		"arm":       newAxiSuperviseArmCmd().Short,
	} {
		for _, provider := range []string{"Codex", "Claude Code"} {
			if !strings.Contains(content, provider) {
				t.Errorf("%s help missing %q: %q", name, provider, content)
			}
		}
	}
}

func TestSupervisionBranchMatchesCurrentWorktree(t *testing.T) {
	repoDir := setupTestRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/current")
	if !supervisionBranchMatches(context.Background(), repoDir, "feature/current") {
		t.Fatal("supervisionBranchMatches() = false, want current branch match")
	}
	if supervisionBranchMatches(context.Background(), repoDir, "feature/other") {
		t.Fatal("supervisionBranchMatches() = true, want branch mismatch rejection")
	}
}

func TestSupervisorHeartbeatDeadlineReusesFutureRegistrationDeadline(t *testing.T) {
	previous := supervisionNow
	supervisionNow = func() time.Time { return time.Unix(1_000, 0) }
	t.Cleanup(func() { supervisionNow = previous })

	if got := supervisorHeartbeatDeadline(supervision.Registration{NextHeartbeatAt: 1_030}); !got.Equal(time.Unix(1_030, 0)) {
		t.Fatalf("supervisorHeartbeatDeadline() = %v, want existing deadline", got)
	}
	if got := supervisorHeartbeatDeadline(supervision.Registration{}); !got.Equal(time.Unix(1_300, 0)) {
		t.Fatalf("supervisorHeartbeatDeadline() = %v, want five-minute deadline", got)
	}
}

func TestAxiSuperviseStatusRendersPersistedRegistration(t *testing.T) {
	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New() error = %v", err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error = %v", err)
	}
	if _, err := supervision.NewStore(p.SupervisionDir()).Arm(supervision.Registration{
		RunID: "run-supervise-status", RepoID: "repo-status", CWD: "/work/status",
	}); err != nil {
		t.Fatalf("Arm() error = %v", err)
	}

	output, err := executeCmd("axi", "supervise", "status", "--run", "run-supervise-status")
	if err != nil {
		t.Fatalf("axi supervise status error = %v", err)
	}
	for _, want := range []string{
		"supervision: armed",
		"run_id: run-supervise-status",
		"session_bound: false",
		"stale_heartbeats: 0",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("axi supervise status output missing %q in:\n%s", want, output)
		}
	}
}

func TestCodexHookIgnoresNonStopEvents(t *testing.T) {
	if err := runAxiCodexHook(strings.NewReader(`{"hook_event_name":"PostToolUse","session_id":"s","turn_id":"t","cwd":"/tmp"}`), io.Discard); err != nil {
		t.Fatalf("runAxiCodexHook() error = %v", err)
	}
}

func TestCodexHookIgnoresMalformedPayload(t *testing.T) {
	if err := runAxiCodexHook(strings.NewReader(`not json`), io.Discard); err != nil {
		t.Fatalf("runAxiCodexHook() error = %v", err)
	}
}

func TestClaudeHookIgnoresMalformedAndNonStopPayloads(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{"hook_event_name":"PostToolUse","session_id":"s","cwd":"/tmp","last_assistant_message":"done"}`,
		`{"hook_event_name":"Stop","session_id":"s","cwd":"/tmp"}`,
	} {
		if err := runAxiClaudeHook(strings.NewReader(raw), io.Discard); err != nil {
			t.Fatalf("runAxiClaudeHook(%q) error = %v", raw, err)
		}
	}
}

func TestClaudeHookHandoffIDIsOpaqueAndChangesPerAssistantTurn(t *testing.T) {
	first := claudeHookHandoffID("session-1", "first completed response")
	second := claudeHookHandoffID("session-1", "second completed response")
	if first == "" || second == "" || first == second {
		t.Fatalf("claude hook IDs = %q, %q; want distinct non-empty opaque IDs", first, second)
	}
	if strings.Contains(first, "session-1") || strings.Contains(first, "first completed response") {
		t.Fatalf("claude hook ID leaks hook payload: %q", first)
	}
	if got := claudeHookHandoffID("session-1", ""); got != "" {
		t.Fatalf("empty assistant message ID = %q, want empty", got)
	}
}

func TestClassifySupervisorRunFailsClosedForAskUserAndMalformedGates(t *testing.T) {
	findings := `{"findings":[{"id":"decision","action":"ask-user"}]}`
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "ask user", raw: findings},
		{name: "malformed", raw: `{`},
		{name: "missing action", raw: `{"findings":[{"id":"unknown"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw
			run := &ipc.RunInfo{ID: "run", Status: types.RunRunning, AwaitingAgent: true, Steps: []ipc.StepResultInfo{{StepName: types.StepReview, Status: types.StepStatusAwaitingApproval, FindingsJSON: &raw}}}
			if got := classifySupervisorRun(run, func(string) []string { return nil }); got != supervisorAskUser {
				t.Fatalf("classifySupervisorRun() = %q, want %q", got, supervisorAskUser)
			}
		})
	}
}

func TestClassifySupervisorRunRecognizesTechnicalGateAndTerminal(t *testing.T) {
	technical := `{"findings":[{"id":"fix","action":"auto-fix"}]}`
	run := &ipc.RunInfo{ID: "run", Status: types.RunRunning, AwaitingAgent: true, Steps: []ipc.StepResultInfo{{StepName: types.StepReview, Status: types.StepStatusAwaitingApproval, FindingsJSON: &technical}}}
	if got := classifySupervisorRun(run, func(string) []string { return nil }); got != supervisorTechnicalGate {
		t.Fatalf("technical gate = %q, want %q", got, supervisorTechnicalGate)
	}
	run.Status = types.RunCompleted
	if got := classifySupervisorRun(run, func(string) []string { return nil }); got != supervisorTerminal {
		t.Fatalf("terminal = %q, want %q", got, supervisorTerminal)
	}
}

func TestApplySupervisorOutcomeBoundsReasonsAndPausesAfterStaleBudget(t *testing.T) {
	for _, budget := range []int{1, 4, 6} {
		t.Run("budget-"+string(rune('0'+budget)), func(t *testing.T) {
			nmHome := t.TempDir()
			p := paths.WithRoot(nmHome)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte("supervision_max_stale_heartbeats: "+string(rune('0'+budget))+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			store := supervision.NewStore(p.SupervisionDir())
			if _, err := store.Arm(supervision.Registration{RunID: "run", RepoID: "repo", CWD: "/work"}); err != nil {
				t.Fatal(err)
			}
			reg, ok, err := store.Claim("/work", "session")
			if err != nil || !ok {
				t.Fatalf("Claim() = (%+v, %v, %v)", reg, ok, err)
			}
			run := &ipc.RunInfo{ID: "run", RepoID: "repo", Status: types.RunRunning, UpdatedAt: 7}
			reg.Fingerprint = supervisorProgressFingerprint(run)
			if err := store.Save(reg); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= budget+1; i++ {
				reg, ok, err = store.Get("run")
				if err != nil || !ok {
					t.Fatalf("Get() = (%+v, %v, %v)", reg, ok, err)
				}
				var out bytes.Buffer
				applySupervisorOutcome(store, p, reg, supervisorHookEvent{SessionID: "session", HandoffID: "turn-" + string(rune('0'+i))}, supervisorHeartbeat, run, &out)
				want := `{"decision":"block","reason":"nm_event=heartbeat"}` + "\n"
				if i == budget+1 {
					want = `{"decision":"block","reason":"nm_event=stale"}` + "\n"
				}
				if got := out.String(); got != want {
					t.Fatalf("heartbeat %d output = %q, want %q", i, got, want)
				}
			}
			reg, ok, err = store.Get("run")
			if err != nil || !ok || reg.Phase != supervision.PhasePaused {
				t.Fatalf("final registration = (%+v, %v, %v), want paused", reg, ok, err)
			}
		})
	}
}
