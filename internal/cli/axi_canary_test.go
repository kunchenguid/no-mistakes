package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAxiCanaryReportsDormantThenActivated proves the operator surface reports a
// dormant canary before activation and the baseline/routed comparison after,
// including the advisory pending target state with empty cohorts.
func TestAxiCanaryReportsDormantThenActivated(t *testing.T) {
	database, _ := setupCanaryTestDB(t)

	dormant := captureCanary(t)
	for _, want := range []string{
		"surface: routing-canary",
		"report_required: true",
		"activated: false",
		"target_advisory: true",
		"comparison_complete: false",
		"result_state: dormant",
		"baseline_complete: false",
		"routed_complete: false",
		"target_met: pending",
	} {
		if !strings.Contains(dormant, want) {
			t.Fatalf("dormant canary missing %q in:\n%s", want, dormant)
		}
	}

	if _, err := database.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	active := captureCanary(t)
	for _, want := range []string{
		"activated: true",
		"report_required: true",
		"target_advisory: true",
		"comparison_complete: false",
		"result_state: preliminary",
		"baseline_complete: false",
		"routed_complete: false",
		"baseline_runs: 0",
		"routed_runs: 0",
		"target_met: pending",
	} {
		if !strings.Contains(active, want) {
			t.Fatalf("activated canary missing %q in:\n%s", want, active)
		}
	}
}

func TestAxiCanaryReportsCompleteExactComparableCohorts(t *testing.T) {
	database, repoID := setupCanaryTestDB(t)
	baselineDurations := []int64{1, 2, 3, 4, 10, 11, 12, 13, 14, 15}
	for i, duration := range baselineDurations {
		seedAxiCanaryRun(t, database, repoID, i, duration)
	}
	if _, err := database.ActivateCanary("fp", func(_, _ string) (int, int, bool) {
		return 2, 20, true
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	routedDurations := []int64{1, 2, 3, 4, 7, 8, 9, 10, 11, 12}
	for i, duration := range routedDurations {
		runID := seedAxiCanaryRun(t, database, repoID, 100+i, duration)
		added, err := database.RecordRoutedRunInCanary(runID, 3, 30)
		if err != nil || !added {
			t.Fatalf("record routed run %d added=%v err=%v", i, added, err)
		}
	}

	out := captureCanary(t)
	for _, want := range []string{
		"report_required: true",
		"target_advisory: true",
		"comparison_complete: true",
		"result_state: complete",
		"baseline_runs: 10",
		"baseline_complete: true",
		"baseline_median_exec_ms: 10.5",
		"baseline_workloads[10]{run,execution_ms,invocation_ms,escalations,failovers,changed_files,changed_lines,initial_findings}:",
		",15,15,0,0,2,20,1",
		"routed_runs: 10",
		"routed_complete: true",
		"routed_median_exec_ms: 7.5",
		"routed_workloads[10]{run,execution_ms,invocation_ms,escalations,failovers,changed_files,changed_lines,initial_findings}:",
		",1,1,0,0,3,30,1",
		"target_met: false",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("complete canary missing %q in:\n%s", want, out)
		}
	}
}

func TestCanaryCohortFieldsExposeExactComparableWorkloadFacts(t *testing.T) {
	cohort := db.CanaryCohort{
		Runs: []db.CanaryRunFacts{
			{
				RunID: "run-a", ExecutionMS: 10, InvocationMS: 8,
				Escalations: 1, ChangedFiles: 2, ChangedLines: 20, InitialFindings: 3,
			},
			{
				RunID: "run-b", ExecutionMS: 11, InvocationMS: 9,
				Escalations: 2, Failovers: 1, ChangedFiles: -1, ChangedLines: -1, InitialFindings: 4,
			},
		},
		Complete:     false,
		MedianExecMS: json.Number("10.5"),
	}
	out := axiDoc(append(
		canaryCohortFields("baseline", cohort),
		canaryCohortFields("routed", cohort)...,
	)...)
	for _, want := range []string{
		"baseline_complete: false",
		"baseline_median_exec_ms: 10.5",
		"baseline_escalations: 3",
		"baseline_failovers: 1",
		"baseline_workloads[2]{run,execution_ms,invocation_ms,escalations,failovers,changed_files,changed_lines,initial_findings}:",
		"run-a,10,8,1,0,2,20,3",
		"run-b,11,9,2,1,-1,-1,4",
		"routed_workloads[2]{run,execution_ms,invocation_ms,escalations,failovers,changed_files,changed_lines,initial_findings}:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("canary workload projection missing %q in:\n%s", want, out)
		}
	}
}

func TestAxiCanaryHelpRequiresReportAndRejectsPreliminaryResults(t *testing.T) {
	help := newAxiCanaryCmd().Long
	for _, want := range []string{"report is required", "preliminary", "must not be treated as live results", "advisory"} {
		if !strings.Contains(help, want) {
			t.Fatalf("canary help missing %q in:\n%s", want, help)
		}
	}
}

func setupCanaryTestDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	repoDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "commit", "--allow-empty", "-m", "initial")
	rawRoot, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		rawRoot = repoDir
	}
	chdir(t, rawRoot)

	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepoWithID("repo-1", rawRoot, "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return database, repo.ID
}

func seedAxiCanaryRun(t *testing.T, database *db.DB, repoID string, sequence int, duration int64) string {
	t.Helper()
	runRecord, err := database.InsertRun(repoID, "feature", fmt.Sprintf("head-%d", sequence), fmt.Sprintf("base-%d", sequence))
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := database.InsertStepResult(runRecord.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert review step: %v", err)
	}
	round, err := database.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatalf("reserve review round: %v", err)
	}
	attemptID, err := database.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: runRecord.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: "review:0:codex",
		Candidate: types.InvocationCandidate{
			Profile: "review", Runner: types.RunnerCodex, Model: "test-model", Effort: types.EffortHigh,
		},
	})
	if err != nil {
		t.Fatalf("start invocation: %v", err)
	}
	if err := database.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{
		Outcome: types.InvocationOutcomeSucceeded, DurationMS: duration,
	}); err != nil {
		t.Fatalf("finish invocation: %v", err)
	}
	findings := `{"findings":[{"id":"f1","severity":"warning","description":"d","action":"auto-fix"}],"summary":"s"}`
	if err := database.CompleteReservedStepRound(round.ID, &findings, nil, duration); err != nil {
		t.Fatalf("complete review round: %v", err)
	}
	if err := database.UpdateRunStatus(runRecord.ID, types.RunCompleted); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	return runRecord.ID
}

func captureCanary(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if err := runAxiCanary(cmd); err != nil {
		t.Fatalf("axi canary: %v\n%s", err, out.String())
	}
	return out.String()
}
