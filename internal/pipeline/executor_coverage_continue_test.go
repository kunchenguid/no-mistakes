package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func coverageFixtureFindings(t *testing.T, run *db.Run, stepID string, reviewed, reviewable []string) string {
	t.Helper()
	f := types.Findings{Items: []types.Finding{}, ReviewedPaths: reviewed, ReviewCoverageScope: &types.ReviewCoverageScope{Version: 1, RunID: run.ID, StepResultID: stepID, HeadSHA: run.HeadSHA, RunBaseSHA: run.BaseSHA, BaseSHA: run.BaseSHA, TrustedConfigSHA: "fixture-trusted", Paths: reviewable}}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func coverageFixtureOutcome(t *testing.T, sctx *StepContext, reviewed, reviewable []string) *StepOutcome {
	return &StepOutcome{NeedsApproval: true, Findings: coverageFixtureFindings(t, sctx.Run, sctx.StepResultID, reviewed, reviewable), ReviewedPaths: reviewed, ReviewablePaths: reviewable, ReviewApprovedHeadSHA: sctx.Run.HeadSHA}
}

func coverageWorkdir(t *testing.T, database *db.DB, run *db.Run) string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := git.Run(ctx, dir, "init"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "first.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".tmp-evidence/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, dir, "add", "first.go", "second.go", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, dir, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	return dir
}

func coverageBinding(t *testing.T, database *db.DB, runID string) *ipc.CoverageBinding {
	t.Helper()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("read disposable review step: %v", err)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil || len(rounds) == 0 {
		t.Fatalf("read disposable review round: %v", err)
	}
	return &ipc.CoverageBinding{HeadSHA: run.HeadSHA, StepResultID: steps[0].ID, RoundID: rounds[len(rounds)-1].ID}
}

func TestExecutor_IncompleteCoverageCanContinueWithoutFixOrApproval(t *testing.T) {
	database, p, run, repo := setupTest(t)
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		if calls == 1 {
			return coverageFixtureOutcome(t, sctx, []string{"first.go"}, []string{"first.go", "second.go"}), nil
		}
		if sctx.Fixing || sctx.FinalizingAnswers {
			t.Error("coverage continuation dispatched a code fix or a fabricated answer")
		}
		return &StepOutcome{
			Findings:              `{"findings":[],"reviewed_paths":["first.go","second.go"]}`,
			ReviewedPaths:         []string{"first.go", "second.go"},
			ReviewablePaths:       []string{"first.go", "second.go"},
			ReviewApprovedHeadSHA: run.HeadSHA,
		}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	workDir := coverageWorkdir(t, database, run)
	if err := os.MkdirAll(filepath.Join(workDir, ".tmp-evidence"), 0o700); err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(workDir, ".tmp-evidence", "retained.txt")
	if err := os.WriteFile(evidencePath, []byte("retained evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- exec.Execute(ctx, run, repo, workDir)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("disposable executor did not exit")
		}
	})
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ApprovalAction("complete-coverage"), nil, nil, nil, nil, "", ResponseOptions{Coverage: coverageBinding(t, database, run.ID)}); err != nil {
		t.Fatalf("cannot complete an empty-findings review without a fix or waiver: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("static review did not resume")
	}
	if calls != 2 {
		t.Fatalf("review calls = %d, want an actual second review before certification", calls)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("review status = %s, want completed after full coverage", steps[0].Status)
	}
	if evidence, err := os.ReadFile(evidencePath); err != nil || string(evidence) != "retained evidence\n" {
		t.Fatalf("coverage continuation changed ignored evidence: %q %v", evidence, err)
	}
}

func TestExecutor_CoverageContinuationRefusesOtherGatesAndOutstandingFindings(t *testing.T) {
	for _, tc := range []struct {
		name string
		step types.StepName
		body string
	}{
		{"test gate", types.StepTest, `{"findings":[]}`},
		{"CI gate", types.StepCI, `{"findings":[]}`},
		{"outstanding finding", types.StepReview, `{"findings":[{"id":"r1","severity":"error","description":"real defect","action":"auto-fix"}]}`},
		{"protected path", types.StepReview, `{"findings":[{"id":"protected-path-refusal","severity":"error","description":"protected path","action":"ask-user"}]}`},
		{"review question", types.StepReview, `{"findings":[{"id":"question-q1","category":"review-question","description":"policy decision","action":"ask-user"}]}`},
		{"unreadable report", types.StepReview, `{`},
		{"omitted findings", types.StepReview, `{}`},
		{"null findings", types.StepReview, `{"findings":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			step := &mockStep{name: tc.step, outcome: &StepOutcome{NeedsApproval: true, Findings: tc.body}}
			exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			workDir := coverageWorkdir(t, database, run)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				_ = exec.Execute(ctx, run, repo, workDir)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("disposable executor did not exit")
				}
			})
			waitForStepStatus(t, database, run.ID, tc.step, types.StepStatusAwaitingApproval)
			if err := exec.Respond(tc.step, types.ApprovalAction("complete-coverage"), nil); err == nil {
				t.Fatal("coverage continuation accepted a gate it cannot resolve")
			}
			if err := exec.Respond(tc.step, types.ActionAbort, nil); err != nil {
				t.Fatalf("refusal consumed the disposable gate: %v", err)
			}
		})
	}
}

func TestExecutor_PersistedFixReviewContinuesCoverageWithoutReplacingItsRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := coverageWorkdir(t, database, run)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	sr, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(sr.ID); err != nil {
		t.Fatal(err)
	}
	body := coverageFixtureFindings(t, run, sr.ID, []string{"first.go"}, []string{"first.go", "second.go"})
	if err := database.SetStepFindings(sr.ID, body); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(sr.ID, 1, "auto_fix", &body, nil, run.HeadSHA, 12); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(sr.ID, types.StepStatusFixReview, 12); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleFixer), "fake", "retained-fixer"); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := database.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if sctx.Run.ID != run.ID || sctx.Fixing || sctx.FinalizingAnswers || sctx.Run.HeadSHA != run.HeadSHA {
			t.Error("persisted coverage recovery replaced the run/head or invoked a fix/answer")
		}
		return &StepOutcome{
			Findings:      `{"findings":[],"reviewed_paths":["first.go","second.go"]}`,
			ReviewedPaths: []string{"first.go", "second.go"}, ReviewablePaths: []string{"first.go", "second.go"},
			ReviewApprovedHeadSHA: run.HeadSHA,
		}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { defer close(finished); done <- exec.Resume(ctx, run, repo, workDir) }()
	t.Cleanup(func() { cancel(); <-finished })
	binding := coverageBinding(t, database, run.ID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := exec.RespondWithOverrides(types.StepReview, types.ActionCompleteCoverage, nil, nil, nil, nil, "", ResponseOptions{Coverage: binding})
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "no step awaiting approval") || time.Now().After(deadline) {
			t.Fatalf("persisted gate did not accept bound static continuation: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("persisted coverage review did not complete")
	}
	after, err := database.GetRoundsByStep(sr.ID)
	if err != nil || len(after) != 2 || after[0].ID != before[0].ID || after[0].FindingsJSON == nil || *after[0].FindingsJSON != body {
		t.Fatalf("coverage continuation lost the original partial report: rounds=%+v err=%v", after, err)
	}
	if after[1].Trigger != "coverage" || after[1].SelectionSource != nil {
		t.Fatalf("static coverage turn was recorded as a fix or verdict: %+v", after[1])
	}
	sessions, err := database.GetRunAgentSessions(run.ID)
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != "retained-fixer" {
		t.Fatalf("coverage continuation changed the retained fixer session: %+v %v", sessions, err)
	}
}

func TestExecutor_CoverageContinuationDoesNotCertifyAnotherPartialReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	reviewedAgain := make(chan struct{})
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		if calls == 2 {
			close(reviewedAgain)
		}
		return coverageFixtureOutcome(t, sctx, []string{"first.go"}, []string{"first.go", "second.go"}), nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	workDir := coverageWorkdir(t, database, run)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = exec.Execute(ctx, run, repo, workDir)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("disposable executor did not exit")
		}
	})
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ApprovalAction("complete-coverage"), nil, nil, nil, nil, "", ResponseOptions{Coverage: coverageBinding(t, database, run.ID)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reviewedAgain:
	case <-time.After(5 * time.Second):
		t.Fatal("static review did not run")
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	select {
	case <-finished:
		t.Fatal("partial review completed without full coverage")
	default:
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.Respond(types.StepReview, types.ActionAbort, nil)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "no step awaiting approval") || time.Now().After(deadline) {
			t.Fatalf("partial review did not return a real gate: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestExecutor_CoverageContinuationRejectsStaleBindingsAndChangedSource(t *testing.T) {
	for _, mutation := range []string{"head", "round", "step", "missing", "dirty", "moved worktree"} {
		t.Run(mutation, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := coverageWorkdir(t, database, run)
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				return coverageFixtureOutcome(t, sctx, nil, []string{"first.go"}), nil
			}}
			exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				_ = exec.Execute(ctx, run, repo, workDir)
			}()
			t.Cleanup(func() { cancel(); <-finished })
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			binding := coverageBinding(t, database, run.ID)
			switch mutation {
			case "head":
				binding.HeadSHA = "stale"
			case "round":
				binding.RoundID = "earlier-round"
			case "step":
				binding.StepResultID = "other-step"
			case "missing":
				binding = nil
			case "dirty", "moved worktree":
				if err := os.WriteFile(filepath.Join(workDir, "first.go"), []byte("package changed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if mutation == "moved worktree" {
					if _, err := git.Run(ctx, workDir, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-am", "other head"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionCompleteCoverage, nil, nil, nil, nil, "", ResponseOptions{Coverage: binding}); err == nil {
				t.Fatal("coverage continuation accepted stale or changed source")
			}
			if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
				t.Fatalf("refusal consumed the disposable gate: %v", err)
			}
		})
	}
}

func TestExecutor_ConcurrentCoverageResponsesStartOnlyOneReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := coverageWorkdir(t, database, run)
	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		if calls == 1 {
			return coverageFixtureOutcome(t, sctx, nil, []string{"first.go"}), nil
		}
		close(started)
		select {
		case <-release:
		case <-sctx.Ctx.Done():
			return nil, sctx.Ctx.Err()
		}
		return &StepOutcome{ReviewedPaths: []string{"first.go"}, ReviewablePaths: []string{"first.go"}}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{TrustedConfigSHA: "fixture-trusted"}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = exec.Execute(ctx, run, repo, workDir)
	}()
	t.Cleanup(func() { cancel(); <-finished })
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	binding := coverageBinding(t, database, run.ID)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := exec.RespondWithOverrides(types.StepReview, types.ActionCompleteCoverage, nil, nil, nil, nil, "", ResponseOptions{Coverage: binding})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d parallel requests, want exactly one", accepted)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("static review did not start")
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionCompleteCoverage, nil, nil, nil, nil, "", ResponseOptions{Coverage: binding}); err == nil {
		t.Fatal("repeated request started another active reviewer")
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("disposable review did not finish")
	}
	if calls != 2 {
		t.Fatalf("review calls = %d, want initial plus one continuation", calls)
	}
}

func TestCanContinueReviewCoverage_ComparesCleanedPaths(t *testing.T) {
	run := &db.Run{ID: "run-1", HeadSHA: "head", BaseSHA: "base"}
	cases := []struct {
		name       string
		reviewed   []string
		reviewable []string
		want       bool
	}{
		{"dot-slash reviewed path matches scope", []string{"./feature.txt"}, []string{"feature.txt", "other.txt"}, true},
		{"dot-slash scope path matches reviewed", []string{"feature.txt"}, []string{"./feature.txt", "other.txt"}, true},
		{"cleaned reviewed paths that cover the scope are complete", []string{"./feature.txt", "dir/../other.txt"}, []string{"feature.txt", "other.txt"}, false},
		{"scope paths that clean to the same file are refused", []string{}, []string{"feature.txt", "./feature.txt"}, false},
		{"reviewed path outside the scope is refused", []string{"./elsewhere.txt"}, []string{"feature.txt", "other.txt"}, false},
		{"reviewed path that cleans to nothing is refused", []string{"./"}, []string{"feature.txt", "other.txt"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := coverageFixtureFindings(t, run, "step-1", tc.reviewed, tc.reviewable)
			if got := CanContinueReviewCoverage(types.StepReview, payload); got != tc.want {
				t.Fatalf("CanContinueReviewCoverage = %v, want %v for %s", got, tc.want, payload)
			}
		})
	}
}
