package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestParseWatchUntilDefaultsToAttentionAndRejectsUnknown(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    watchUntil
		wantErr bool
	}{
		{name: "default", want: watchUntilAttention},
		{name: "attention", input: "attention", want: watchUntilAttention},
		{name: "terminal", input: "terminal", want: watchUntilTerminal},
		{name: "unknown", input: "forever", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseWatchUntil(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("parseWatchUntil() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWatchUntil() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseWatchUntil() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWatchQuietDelayUsesNearestActiveStep(t *testing.T) {
	previous := watchNow
	watchNow = func() time.Time { return time.Unix(1_000, 0) }
	t.Cleanup(func() { watchNow = previous })

	first, second := int64(995), int64(997)
	rv := runView{Steps: []stepView{
		{Name: "review", Status: "running", LastActivityAt: &first},
		{Name: "test", Status: "fixing", LastActivityAt: &second},
	}}

	if got := watchQuietDelay(rv, 10*time.Second); got != 5*time.Second {
		t.Fatalf("watchQuietDelay() = %v, want 5s", got)
	}
}

func TestWatchRunViewUsesLogActivityFallback(t *testing.T) {
	previous := watchNow
	watchNow = func() time.Time { return time.Unix(1_000, 0) }
	t.Cleanup(func() { watchNow = previous })

	p := paths.WithRoot(t.TempDir())
	if err := os.MkdirAll(p.RunLogDir("run-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(p.RunLogDir("run-1"), "review.log")
	if err := os.WriteFile(logPath, []byte("legacy activity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(logPath, time.Unix(990, 0), time.Unix(990, 0)); err != nil {
		t.Fatal(err)
	}

	run := &ipc.RunInfo{ID: "run-1", Status: types.RunRunning, Steps: []ipc.StepResultInfo{{StepName: types.StepReview, Status: types.StepStatusRunning}}}
	if done, reason := watchReason(watchRunView(p, run), 10*time.Second, func(string) []string { return nil }); !done || reason != "quiet" {
		t.Fatalf("watchReason() = (%v, %q), want (true, quiet)", done, reason)
	}
}

func TestWatchRunViewSchedulesQuietTimerFromLogActivityFallback(t *testing.T) {
	previous := watchNow
	watchNow = func() time.Time { return time.Unix(1_000, 0) }
	t.Cleanup(func() { watchNow = previous })

	p := paths.WithRoot(t.TempDir())
	if err := os.MkdirAll(p.RunLogDir("run-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(p.RunLogDir("run-1"), "review.log")
	if err := os.WriteFile(logPath, []byte("legacy activity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(logPath, time.Unix(995, 0), time.Unix(995, 0)); err != nil {
		t.Fatal(err)
	}

	run := &ipc.RunInfo{ID: "run-1", Status: types.RunRunning, Steps: []ipc.StepResultInfo{{StepName: types.StepReview, Status: types.StepStatusRunning}}}
	rv := watchRunView(p, run)
	if done, reason := watchReason(rv, 10*time.Second, func(string) []string { return nil }); done || reason != "" {
		t.Fatalf("watchReason() = (%v, %q), want (false, empty)", done, reason)
	}
	if got := watchQuietDelay(rv, 10*time.Second); got != 5*time.Second {
		t.Fatalf("watchQuietDelay() = %v, want 5s", got)
	}
}

func TestWatchResultFingerprintIncludesStopOutcomeAndRunState(t *testing.T) {
	rv := runView{ID: "run-1", Branch: "feature/watch", Status: "running", Steps: []stepView{{Name: "ci", Status: "running"}}}
	baseline := watchResultFingerprint(watchUntilAttention, rv, "quiet")
	if changed := watchResultFingerprint(watchUntilAttention, rv, "checks-passed"); changed == baseline {
		t.Fatal("changing the watch stop outcome must change the telemetry fingerprint")
	}
	rv.Steps[0].Status = "completed"
	if changed := watchResultFingerprint(watchUntilAttention, rv, "quiet"); changed == baseline {
		t.Fatal("changing the watched run state must change the telemetry fingerprint")
	}
	if changed := watchResultFingerprint(watchUntilTerminal, rv, "quiet"); changed == baseline {
		t.Fatal("changing the watch mode must change the telemetry fingerprint")
	}
}

func TestRenderWatchResultBoundsGateFindings(t *testing.T) {
	items := make([]types.Finding, maxWatchFindings+1)
	for i := range items {
		items[i] = types.Finding{ID: fmt.Sprintf("F%d", i+1), Severity: "medium", File: "file.go", Action: string(types.ActionFix), Description: "needs attention"}
	}
	findingsJSON, err := types.MarshalFindingsJSON(types.Findings{Items: items})
	if err != nil {
		t.Fatalf("encode findings: %v", err)
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err = renderWatchResult(cmd, runView{
		ID:     "run-1",
		Status: "running",
		Steps:  []stepView{{Name: string(types.StepReview), Status: string(types.StepStatusAwaitingApproval), FindingsJSON: findingsJSON}},
	}, "gate")
	if err != nil {
		t.Fatalf("renderWatchResult() error = %v", err)
	}
	got := out.String()
	for _, want := range []string{"findings_total: 11", "findings_truncated: true", "F10", "no-mistakes axi logs --step review --full"} {
		if !strings.Contains(got, want) {
			t.Errorf("watch output missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "F11") {
		t.Errorf("watch output included an unbounded finding:\n%s", got)
	}
}

func TestRenderWatchResultIncludesTerminalError(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := renderWatchResult(cmd, runView{ID: "run-1", Status: "failed", Error: "review failed"}, "terminal")
	if err == nil {
		t.Fatal("failed terminal run must return an error exit")
	}
	for _, want := range []string{"outcome: failed", "error: review failed", "stop: terminal", "supervision: active_agent_required"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("watch output missing %q in:\n%s", want, out.String())
		}
	}
}

func TestAxiWatchCommandReportsTerminalFailure(t *testing.T) {
	repoDir := setupTestRepo(t)
	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New() error = %v", err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error = %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	repo, err := database.InsertRepoWithID("repo-watch", repoDir, "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "feature/watch", "abcdef1234567890", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunError(run.ID, "review failed"); err != nil {
		t.Fatalf("mark run failed: %v", err)
	}

	output, err := executeCmd("axi", "watch", "--run", run.ID)
	if err == nil {
		t.Fatal("axi watch error = nil, want terminal failure exit")
	}
	if exit, ok := err.(*exitError); !ok || exit.code != 1 {
		t.Fatalf("axi watch error = %#v, want exit code 1", err)
	}
	for _, want := range []string{
		"status: failed",
		"stop: terminal",
		"terminal: true",
		"supervision: active_agent_required",
		"auto_resumed: false",
		"outcome: failed",
		"error: review failed",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("axi watch output missing %q in:\n%s", want, output)
		}
	}
	t.Logf("end-user axi watch transcript:\n%s", output)
}
