package pipeline

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const boundCIGateFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"repair CI","action":"ask-user","category":"ci-check","check":"test"}]}`

func boundProtectedPathFindings() string {
	return ProtectedPathOutcome(&ProtectedPathError{Path: "package.lock", Rule: "*.lock"}).Findings
}

func newBoundWorktree(t *testing.T, database *db.DB, run *db.Run) string {
	t.Helper()
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	head, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	execGit(t, workDir, "checkout", "--detach", head)
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	return workDir
}

type blockingRecoveredCIStep struct {
	*adaptiveCallStep
	started chan struct{}
	release chan struct{}
}

func (s *blockingRecoveredCIStep) ReconcileApprovalGate(*StepContext) (bool, error) {
	close(s.started)
	<-s.release
	return false, nil
}

func waitForExecutorRound(t *testing.T, database *db.DB, exec *Executor, stepResultID string, minimumRounds int) *db.StepRound {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rounds, err := database.GetRoundsByStep(stepResultID)
		if err == nil && len(rounds) >= minimumRounds {
			latest := rounds[len(rounds)-1]
			exec.mu.Lock()
			waiting := exec.waiting && exec.waitingStep == types.StepCI && exec.waitingRoundID == latest.ID
			exec.mu.Unlock()
			if waiting {
				return latest
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("executor did not park on CI round %d", minimumRounds)
	return nil
}

func TestExecutor_BoundCIResponseForcesFreshValidationBeforeTheNextPush(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := newBoundWorktree(t, database, run)

	var order []types.StepName
	pass := func(name types.StepName) Step {
		return &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) {
			order = append(order, name)
			return &StepOutcome{}, nil
		}}
	}
	push := &adaptiveCallStep{name: types.StepPush, fn: func(*StepContext) (*StepOutcome, error) {
		order = append(order, types.StepPush)
		if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"}); err != nil {
			return nil, err
		}
		return &StepOutcome{}, nil
	}}
	pr := &adaptiveCallStep{name: types.StepPR, fn: func(*StepContext) (*StepOutcome, error) {
		order = append(order, types.StepPR)
		if err := database.UpdateRunPRURL(run.ID, "https://example.invalid/repo/pull/1"); err != nil {
			return nil, err
		}
		return &StepOutcome{}, nil
	}}
	ciCalls := 0
	ci := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		order = append(order, types.StepCI)
		ciCalls++
		switch ciCalls {
		case 1:
			return &StepOutcome{NeedsApproval: true, Findings: boundCIGateFindings}, nil
		case 2:
			if !sctx.Fixing || !sctx.RequireReviewRevalidation || sctx.PreviousFindings == "" {
				t.Errorf("bound fix context = fixing:%v require_review:%v previous:%q", sctx.Fixing, sctx.RequireReviewRevalidation, sctx.PreviousFindings)
			}
			findings, err := types.ParseFindingsJSON(sctx.PreviousFindings)
			if err != nil {
				t.Errorf("parse bound fix findings: %v", err)
			} else if len(findings.Items) != 2 || findings.Items[0].ID != "ci-1" || findings.Items[1].ID != "user-1" || findings.Items[1].UserInstructions != "change source only; cancellation is not a green verdict" {
				t.Errorf("bound fix findings = %#v, want selected cancellation plus explicit source correction", findings.Items)
			}
			return &StepOutcome{RestartFrom: types.StepReview}, nil
		case 3:
			if sctx.Fixing {
				t.Error("post-revalidation CI observation ran as a fix round")
			}
			return &StepOutcome{
				AutoFixable: true,
				Findings:    `{"findings":[{"id":"later-ci-1","severity":"error","description":"later ordinary CI failure","action":"auto-fix"}]}`,
			}, nil
		default:
			if !sctx.Fixing || sctx.RequireReviewRevalidation {
				t.Errorf("later ordinary auto-fix context = fixing:%v require_review:%v", sctx.Fixing, sctx.RequireReviewRevalidation)
			}
			return &StepOutcome{}, nil
		}
	}}
	cycle := []Step{pass(types.StepReview), pass(types.StepTest), pass(types.StepDocument), pass(types.StepLint), push, pr}
	steps := append(append([]Step(nil), cycle...), ci)
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{CI: 1}}, nil, steps, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)

	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ciResult *db.StepResult
	for _, result := range results {
		if result.StepName == types.StepCI {
			ciResult = result
		}
	}
	if ciResult == nil {
		t.Fatal("CI result missing")
	}
	rounds, err := database.GetRoundsByStep(ciResult.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("CI rounds = %#v, error = %v", rounds, err)
	}
	binding := ResponseRevalidationBinding{
		OperationID:  "operation-1",
		Fingerprint:  "sha256:request-1",
		RunID:        run.ID,
		RepoID:       repo.ID,
		Branch:       run.Branch,
		HeadSHA:      run.HeadSHA,
		StepResultID: ciResult.ID,
		RoundID:      rounds[0].ID,
	}
	added := []types.Finding{{Description: "apply the bounded source correction", Action: types.ActionAutoFix, UserInstructions: "change source only; cancellation is not a green verdict"}}
	dispositions, operation, replayed, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, added, "", binding)
	if err != nil {
		t.Fatalf("bound response: %v", err)
	}
	if replayed || operation == nil || operation.OperationID != binding.OperationID {
		t.Fatalf("receipt = %#v replayed=%v", operation, replayed)
	}
	if strings.Join(dispositions.Fixed, ",") != "ci-1,user-1" {
		t.Fatalf("dispositions = %#v", dispositions)
	}
	waitExecutorDone(t, done)

	want := []types.StepName{
		types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI,
		types.StepCI,
		types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI,
		types.StepCI,
	}
	if !slices.Equal(order, want) {
		t.Fatalf("execution order = %v, want %v", order, want)
	}

	dispositions, replayedOperation, replayed, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, added, "", binding)
	if err != nil || !replayed || replayedOperation == nil || replayedOperation.AcceptedAt != operation.AcceptedAt || !slices.Equal(dispositions.Fixed, []string{"ci-1", "user-1"}) {
		t.Fatalf("identical uncertain retry: dispositions=%#v operation=%#v replayed=%v error=%v", dispositions, replayedOperation, replayed, err)
	}
	changed := binding
	changed.Fingerprint = "sha256:changed-request"
	if _, _, _, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, added, "", changed); err == nil || !strings.Contains(err.Error(), "different response") {
		t.Fatalf("changed retry error = %v, want immutable-operation refusal", err)
	}
}

func TestExecutor_BoundCIResponseSurvivesRepeatedProtectedPathRetries(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := newBoundWorktree(t, database, run)

	var order []types.StepName
	pass := func(name types.StepName) Step {
		return &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) {
			order = append(order, name)
			return &StepOutcome{}, nil
		}}
	}
	push := &adaptiveCallStep{name: types.StepPush, fn: func(*StepContext) (*StepOutcome, error) {
		order = append(order, types.StepPush)
		if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"}); err != nil {
			return nil, err
		}
		return &StepOutcome{}, nil
	}}
	pr := &adaptiveCallStep{name: types.StepPR, fn: func(*StepContext) (*StepOutcome, error) {
		order = append(order, types.StepPR)
		if err := database.UpdateRunPRURL(run.ID, "https://example.invalid/repo/pull/1"); err != nil {
			return nil, err
		}
		return &StepOutcome{}, nil
	}}
	ciCalls := 0
	ci := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		order = append(order, types.StepCI)
		ciCalls++
		switch ciCalls {
		case 1:
			return &StepOutcome{NeedsApproval: true, Findings: boundCIGateFindings}, nil
		case 2, 3:
			if !sctx.Fixing || !sctx.RequireReviewRevalidation {
				t.Errorf("protected retry %d context = fixing:%v require_review:%v", ciCalls-1, sctx.Fixing, sctx.RequireReviewRevalidation)
			}
			return &StepOutcome{NeedsApproval: true, Findings: boundProtectedPathFindings()}, nil
		case 4:
			if !sctx.Fixing || !sctx.RequireReviewRevalidation {
				t.Errorf("retained repair context = fixing:%v require_review:%v", sctx.Fixing, sctx.RequireReviewRevalidation)
			}
			return &StepOutcome{RestartFrom: types.StepReview}, nil
		default:
			if sctx.Fixing || sctx.RequireReviewRevalidation {
				t.Errorf("post-revalidation CI context = fixing:%v require_review:%v", sctx.Fixing, sctx.RequireReviewRevalidation)
			}
			return &StepOutcome{}, nil
		}
	}}
	cycle := []Step{pass(types.StepReview), pass(types.StepTest), pass(types.StepDocument), pass(types.StepLint), push, pr}
	steps := append(append([]Step(nil), cycle...), ci)
	exec := NewExecutor(database, p, nil, nil, steps, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)

	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ciResult *db.StepResult
	for _, result := range results {
		if result.StepName == types.StepCI {
			ciResult = result
		}
	}
	if ciResult == nil {
		t.Fatal("CI result missing")
	}
	firstRound := waitForExecutorRound(t, database, exec, ciResult.ID, 1)
	binding := ResponseRevalidationBinding{
		OperationID: "operation-protected-live", Fingerprint: "sha256:protected-live", RunID: run.ID,
		RepoID: repo.ID, Branch: run.Branch, HeadSHA: run.HeadSHA, StepResultID: ciResult.ID, RoundID: firstRound.ID,
	}
	if _, operation, replayed, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding); err != nil || operation == nil || replayed {
		t.Fatalf("bound response: operation=%#v replayed=%v err=%v", operation, replayed, err)
	}

	for roundCount := 2; roundCount <= 3; roundCount++ {
		waitForExecutorRound(t, database, exec, ciResult.ID, roundCount)
		if _, err := exec.RespondWithOverrides(types.StepCI, types.ActionFix, []string{"protected-path-refusal"}, nil, nil, nil, ""); err != nil {
			t.Fatalf("protected-path retry %d: %v", roundCount-1, err)
		}
	}
	waitExecutorDone(t, done)

	want := []types.StepName{
		types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI,
		types.StepCI, types.StepCI, types.StepCI,
		types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI,
	}
	if !slices.Equal(order, want) {
		t.Fatalf("execution order = %v, want %v", order, want)
	}
}

func TestExecutor_ConcurrentIdenticalBoundResponsesDispatchOnceAndReplayOnce(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := newBoundWorktree(t, database, run)
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://example.invalid/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	for _, step := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		result, err := database.InsertStepResult(run.ID, step)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateStepStatus(result.ID, types.StepStatusCompleted); err != nil {
			t.Fatal(err)
		}
	}
	ciResult, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertStepRound(ciResult.ID, 1, "initial", stringPointer(boundCIGateFindings), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, ciResult.ID, types.StepStatusAwaitingApproval, 1, 1, stringPointer(boundCIGateFindings)); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(database, p, nil, nil, nil, nil)
	exec.workDir = workDir
	exec.waiting = true
	exec.waitingStep = types.StepCI
	exec.waitingStepResultID = ciResult.ID
	exec.waitingRoundID = round.ID
	exec.waitingFindings = boundCIGateFindings
	exec.approvalCh = make(chan approvalResponse, 2)
	binding := ResponseRevalidationBinding{
		OperationID: "operation-concurrent", Fingerprint: "sha256:concurrent", RunID: run.ID,
		RepoID: repo.ID, Branch: run.Branch, HeadSHA: run.HeadSHA, StepResultID: ciResult.ID, RoundID: round.ID,
	}

	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	exec.beforeBoundResponseLock = func() {
		arrived <- struct{}{}
		<-release
	}
	type result struct {
		dispositions RespondDispositions
		operation    *db.BoundResponseOperation
		replayed     bool
		err          error
	}
	results := make(chan result, 2)
	var callers sync.WaitGroup
	for range 2 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			dispositions, operation, replayed, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding)
			results <- result{dispositions: dispositions, operation: operation, replayed: replayed, err: err}
		}()
	}
	<-arrived
	<-arrived
	close(release)
	callers.Wait()
	close(results)

	replays := 0
	for got := range results {
		if got.err != nil || got.operation == nil || got.operation.OperationID != binding.OperationID || !slices.Equal(got.dispositions.Fixed, []string{"ci-1"}) {
			t.Fatalf("concurrent response = %#v, operation=%#v, replayed=%v, error=%v", got.dispositions, got.operation, got.replayed, got.err)
		}
		if got.replayed {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("replayed responses = %d, want exactly one", replays)
	}
	if len(exec.approvalCh) != 1 {
		t.Fatalf("approval dispatches = %d, want exactly one", len(exec.approvalCh))
	}
}

func TestExecutor_BoundCIResponseRefusesLiveWorktreeRacesBeforeReceipt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "descendant head", mutate: func(t *testing.T, workDir string) {
			writeTestFile(t, workDir, "race.txt", "descendant\n")
			execGit(t, workDir, "add", "race.txt")
			execGit(t, workDir, "commit", "-m", "out-of-band descendant")
		}},
		{name: "dirty tracked work", mutate: func(t *testing.T, workDir string) {
			writeTestFile(t, workDir, "README.md", "dirty out-of-band edit\n")
		}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := newBoundWorktree(t, database, run)
			if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"}); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPRURL(run.ID, "https://example.invalid/repo/pull/1"); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			for _, step := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
				result, err := database.InsertStepResult(run.ID, step)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateStepStatus(result.ID, types.StepStatusCompleted); err != nil {
					t.Fatal(err)
				}
			}
			ciResult, err := database.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			round, err := database.InsertStepRound(ciResult.ID, 1, "initial", stringPointer(boundCIGateFindings), nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, ciResult.ID, types.StepStatusAwaitingApproval, 1, 1, stringPointer(boundCIGateFindings)); err != nil {
				t.Fatal(err)
			}
			exec := NewExecutor(database, p, nil, nil, nil, nil)
			exec.workDir = workDir
			exec.waiting = true
			exec.waitingStep = types.StepCI
			exec.waitingStepResultID = ciResult.ID
			exec.waitingRoundID = round.ID
			exec.waitingFindings = boundCIGateFindings
			exec.approvalCh = make(chan approvalResponse, 1)
			binding := ResponseRevalidationBinding{
				OperationID: "operation-worktree-race-" + strings.ReplaceAll(tc.name, " ", "-"), Fingerprint: "sha256:" + strings.ReplaceAll(tc.name, " ", "-"),
				RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, HeadSHA: run.HeadSHA, StepResultID: ciResult.ID, RoundID: round.ID,
			}
			exec.beforeBoundResponseLock = func() { tc.mutate(t, workDir) }

			if _, _, _, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding); err == nil || (!strings.Contains(err.Error(), "live worktree head") && !strings.Contains(err.Error(), "uncommitted changes")) {
				t.Fatalf("worktree race error = %v", err)
			}
			if !exec.waiting || len(exec.approvalCh) != 0 {
				t.Fatalf("worktree race released gate: waiting=%v dispatches=%d", exec.waiting, len(exec.approvalCh))
			}
			if operation, err := database.GetBoundResponseOperation(binding.OperationID); err != nil || operation != nil {
				t.Fatalf("worktree race recorded operation %#v, error=%v", operation, err)
			}
			storedRounds, err := database.GetRoundsByStep(ciResult.ID)
			if err != nil || len(storedRounds) != 1 || storedRounds[0].SelectedFindingIDs != nil || storedRounds[0].UserFindingsJSON != nil {
				t.Fatalf("worktree race changed rounds: %#v error=%v", storedRounds, err)
			}
		})
	}
}

func TestExecutor_BoundCIFixRechecksLiveWorktreeAtInvocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "descendant head", mutate: func(t *testing.T, workDir string) {
			writeTestFile(t, workDir, "fix-race.txt", "descendant\n")
			execGit(t, workDir, "add", "fix-race.txt")
			execGit(t, workDir, "commit", "-m", "race before bound fixer")
		}},
		{name: "dirty tracked work", mutate: func(t *testing.T, workDir string) {
			writeTestFile(t, workDir, "README.md", "dirty before bound fixer\n")
		}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := newBoundWorktree(t, database, run)
			pass := func(name types.StepName) Step {
				return &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) { return &StepOutcome{}, nil }}
			}
			push := &adaptiveCallStep{name: types.StepPush, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{}, database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"})
			}}
			pr := &adaptiveCallStep{name: types.StepPR, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{}, database.UpdateRunPRURL(run.ID, "https://example.invalid/repo/pull/1")
			}}
			ciCalls := 0
			ci := &adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
				ciCalls++
				if ciCalls == 1 {
					return &StepOutcome{NeedsApproval: true, Findings: boundCIGateFindings}, nil
				}
				t.Errorf("bound fixer ran after the live worktree race")
				return &StepOutcome{}, nil
			}}
			exec := NewExecutor(database, p, nil, nil, []Step{pass(types.StepReview), pass(types.StepTest), pass(types.StepDocument), pass(types.StepLint), push, pr, ci}, nil)
			done, _ := startExecutor(t, exec, run, repo, workDir)
			waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
			results, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var ciResult *db.StepResult
			for _, result := range results {
				if result.StepName == types.StepCI {
					ciResult = result
				}
			}
			if ciResult == nil {
				t.Fatal("CI result missing")
			}
			round := waitForExecutorRound(t, database, exec, ciResult.ID, 1)
			binding := ResponseRevalidationBinding{
				OperationID: "operation-fix-race-" + strings.ReplaceAll(tc.name, " ", "-"), Fingerprint: "sha256:fix-" + strings.ReplaceAll(tc.name, " ", "-"),
				RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, HeadSHA: run.HeadSHA, StepResultID: ciResult.ID, RoundID: round.ID,
			}
			exec.beforeBoundFixExecution = func() { tc.mutate(t, workDir) }
			if _, operation, replayed, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding); err != nil || operation == nil || replayed {
				t.Fatalf("accept bound response: operation=%#v replayed=%v error=%v", operation, replayed, err)
			}
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "refusing bound CI fix") {
					t.Fatalf("bound fixer race result = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("bound fixer race did not fail the run")
			}
			if ciCalls != 1 {
				t.Fatalf("CI executions = %d, want only the parked round", ciCalls)
			}
			if operation, err := database.GetBoundResponseOperation(binding.OperationID); err != nil || operation == nil {
				t.Fatalf("accepted operation receipt = %#v error=%v", operation, err)
			}
		})
	}
}

func TestExecutor_BoundCIProtectedRetryStillRechecksHeadAndDetachment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{name: "descendant head", want: "live worktree head", mutate: func(t *testing.T, workDir string) {
			writeTestFile(t, workDir, "retained-race.txt", "descendant\n")
			execGit(t, workDir, "add", "retained-race.txt")
			execGit(t, workDir, "commit", "-m", "race before retained repair")
		}},
		{name: "attached worktree", want: "no longer detached", mutate: func(t *testing.T, workDir string) {
			execGit(t, workDir, "checkout", "-b", "retained-race-attached")
		}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := newBoundWorktree(t, database, run)
			pass := func(name types.StepName) Step {
				return &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) { return &StepOutcome{}, nil }}
			}
			push := &adaptiveCallStep{name: types.StepPush, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{}, database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"})
			}}
			pr := &adaptiveCallStep{name: types.StepPR, fn: func(*StepContext) (*StepOutcome, error) {
				return &StepOutcome{}, database.UpdateRunPRURL(run.ID, "https://example.invalid/repo/pull/1")
			}}
			ciCalls := 0
			ci := &adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
				ciCalls++
				switch ciCalls {
				case 1:
					return &StepOutcome{NeedsApproval: true, Findings: boundCIGateFindings}, nil
				case 2:
					return &StepOutcome{NeedsApproval: true, Findings: boundProtectedPathFindings()}, nil
				default:
					t.Errorf("retained protected-path fixer ran after %s race", tc.name)
					return &StepOutcome{}, nil
				}
			}}
			exec := NewExecutor(database, p, nil, nil, []Step{pass(types.StepReview), pass(types.StepTest), pass(types.StepDocument), pass(types.StepLint), push, pr, ci}, nil)
			done, _ := startExecutor(t, exec, run, repo, workDir)
			waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
			results, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var ciResult *db.StepResult
			for _, result := range results {
				if result.StepName == types.StepCI {
					ciResult = result
				}
			}
			if ciResult == nil {
				t.Fatal("CI result missing")
			}
			firstRound := waitForExecutorRound(t, database, exec, ciResult.ID, 1)
			binding := ResponseRevalidationBinding{
				OperationID: "operation-retained-race-" + strings.ReplaceAll(tc.name, " ", "-"), Fingerprint: "sha256:retained-race-" + strings.ReplaceAll(tc.name, " ", "-"),
				RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, HeadSHA: run.HeadSHA, StepResultID: ciResult.ID, RoundID: firstRound.ID,
			}
			if _, operation, replayed, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding); err != nil || operation == nil || replayed {
				t.Fatalf("accept bound response: operation=%#v replayed=%v error=%v", operation, replayed, err)
			}
			waitForExecutorRound(t, database, exec, ciResult.ID, 2)
			exec.beforeBoundFixExecution = func() { tc.mutate(t, workDir) }
			if _, err := exec.RespondWithOverrides(types.StepCI, types.ActionFix, []string{"protected-path-refusal"}, nil, nil, nil, ""); err != nil {
				t.Fatalf("protected-path retry: %v", err)
			}
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "refusing bound CI fix") || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("retained fixer race result = %v, want %q", err, tc.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("retained fixer race did not fail the run")
			}
			if ciCalls != 2 {
				t.Fatalf("CI executions = %d, want parked round and first protected refusal", ciCalls)
			}
		})
	}
}

func TestExecutor_BoundCIResponseSurvivesAnAcceptedReplyInterruption(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := newBoundWorktree(t, database, run)
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://example.invalid/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	for _, step := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		result, err := database.InsertStepResult(run.ID, step)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateStepStatus(result.ID, types.StepStatusCompleted); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []types.StepName{types.StepPush, types.StepPR} {
		result, err := database.InsertStepResult(run.ID, step)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateStepStatus(result.ID, types.StepStatusCompleted); err != nil {
			t.Fatal(err)
		}
	}
	ciResult, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(ciResult.ID); err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertStepRound(ciResult.ID, 1, "initial", stringPointer(boundCIGateFindings), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, ciResult.ID, types.StepStatusAwaitingApproval, 1, 1, stringPointer(boundCIGateFindings)); err != nil {
		t.Fatal(err)
	}
	binding := ResponseRevalidationBinding{
		OperationID: "operation-interrupted", Fingerprint: "sha256:interrupted", RunID: run.ID,
		RepoID: repo.ID, Branch: run.Branch, HeadSHA: run.HeadSHA, StepResultID: ciResult.ID, RoundID: round.ID,
	}
	crashed := NewExecutor(database, p, nil, nil, nil, nil)
	crashed.workDir = workDir
	crashed.waiting = true
	crashed.waitingStep = types.StepCI
	crashed.waitingStepResultID = ciResult.ID
	crashed.waitingRoundID = round.ID
	crashed.waitingFindings = boundCIGateFindings
	crashed.approvalCh = make(chan approvalResponse, 1)
	if _, operation, replayed, err := crashed.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding); err != nil || operation == nil || replayed {
		t.Fatalf("accepted response before interruption: operation=%#v replayed=%v error=%v", operation, replayed, err)
	}
	// Simulate loss of the caller reply and daemon process before its buffered
	// action was consumed. Resume must reconstruct the parked gate itself; an
	// identical retry then dispatches exactly that recorded response and carries
	// the one-shot revalidation demand into recovered execution.
	var order []types.StepName
	pass := func(name types.StepName) Step {
		return &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) {
			order = append(order, name)
			return &StepOutcome{}, nil
		}}
	}
	ciCalls := 0
	ciStep := &blockingRecoveredCIStep{adaptiveCallStep: &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		order = append(order, types.StepCI)
		ciCalls++
		if ciCalls == 1 {
			if !sctx.Fixing || !sctx.RequireReviewRevalidation || !strings.Contains(sctx.PreviousFindings, "ci-1") {
				t.Errorf("recovered bound fix context = fixing:%v require_review:%v previous:%q", sctx.Fixing, sctx.RequireReviewRevalidation, sctx.PreviousFindings)
			}
			return &StepOutcome{RestartFrom: types.StepReview}, nil
		}
		if sctx.Fixing || sctx.RequireReviewRevalidation {
			t.Errorf("post-revalidation CI context = fixing:%v require_review:%v", sctx.Fixing, sctx.RequireReviewRevalidation)
		}
		return &StepOutcome{}, nil
	}}, started: make(chan struct{}), release: make(chan struct{})}
	steps := []Step{
		pass(types.StepReview), pass(types.StepTest), pass(types.StepDocument), pass(types.StepLint),
		pass(types.StepPush), pass(types.StepPR), ciStep,
	}
	recovered := NewExecutor(database, p, nil, nil, steps, nil)
	recovered.PrepareRecoveredRun()
	recoveredRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- recovered.Resume(context.Background(), recoveredRun, repo, workDir) }()
	<-ciStep.started
	type retryResult struct {
		operation *db.BoundResponseOperation
		replayed  bool
		err       error
	}
	retry := make(chan retryResult, 1)
	go func() {
		_, operation, replayed, err := recovered.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding)
		retry <- retryResult{operation: operation, replayed: replayed, err: err}
	}()
	select {
	case result := <-retry:
		t.Fatalf("retry returned before recovered gate readiness: %#v", result)
	case <-time.After(100 * time.Millisecond):
	}
	close(ciStep.release)
	result := <-retry
	if result.err != nil || result.operation == nil || result.replayed {
		t.Fatalf("recovered retry: operation=%#v replayed=%v error=%v", result.operation, result.replayed, result.err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Resume after accepted reply interruption: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovered executor did not finish")
	}
	wantOrder := []types.StepName{
		types.StepCI,
		types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI,
	}
	if !slices.Equal(order, wantOrder) {
		t.Fatalf("recovered execution order = %v, want %v", order, wantOrder)
	}
	durable, err := database.GetBoundResponseOperation(binding.OperationID)
	if err != nil || durable == nil || durable.RoundID != round.ID {
		t.Fatalf("recovered operation receipt = %#v, error = %v", durable, err)
	}
}

func TestExecutor_RecoveredProtectedPathRetryRetainsBoundRevalidation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := newBoundWorktree(t, database, run)
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://example.invalid/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	for _, step := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR} {
		result, err := database.InsertStepResult(run.ID, step)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateStepStatus(result.ID, types.StepStatusCompleted); err != nil {
			t.Fatal(err)
		}
	}
	ciResult, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(ciResult.ID); err != nil {
		t.Fatal(err)
	}
	firstRound, err := database.InsertStepRound(ciResult.ID, 1, "initial", stringPointer(boundCIGateFindings), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, ciResult.ID, types.StepStatusAwaitingApproval, 1, 1, stringPointer(boundCIGateFindings)); err != nil {
		t.Fatal(err)
	}
	binding := ResponseRevalidationBinding{
		OperationID: "operation-protected-recovered", Fingerprint: "sha256:protected-recovered", RunID: run.ID,
		RepoID: repo.ID, Branch: run.Branch, HeadSHA: run.HeadSHA, StepResultID: ciResult.ID, RoundID: firstRound.ID,
	}
	acceptor := NewExecutor(database, p, nil, nil, nil, nil)
	acceptor.workDir = workDir
	acceptor.waiting = true
	acceptor.waitingStep = types.StepCI
	acceptor.waitingStepResultID = ciResult.ID
	acceptor.waitingRoundID = firstRound.ID
	acceptor.waitingFindings = boundCIGateFindings
	acceptor.approvalCh = make(chan approvalResponse, 1)
	if _, operation, replayed, err := acceptor.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding); err != nil || operation == nil || replayed {
		t.Fatalf("accept bound response: operation=%#v replayed=%v err=%v", operation, replayed, err)
	}

	protected := boundProtectedPathFindings()
	secondRound, err := database.InsertStepRound(ciResult.ID, 2, "fix", &protected, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, ciResult.ID, types.StepStatusFixReview, 1, 1, &protected); err != nil {
		t.Fatal(err)
	}

	var order []types.StepName
	pass := func(name types.StepName) Step {
		return &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) {
			order = append(order, name)
			return &StepOutcome{}, nil
		}}
	}
	ciCalls := 0
	ci := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		order = append(order, types.StepCI)
		ciCalls++
		if ciCalls == 1 {
			if !sctx.Fixing || !sctx.RequireReviewRevalidation {
				t.Errorf("recovered protected retry context = fixing:%v require_review:%v", sctx.Fixing, sctx.RequireReviewRevalidation)
			}
			return &StepOutcome{RestartFrom: types.StepReview}, nil
		}
		if sctx.Fixing || sctx.RequireReviewRevalidation {
			t.Errorf("post-revalidation CI context = fixing:%v require_review:%v", sctx.Fixing, sctx.RequireReviewRevalidation)
		}
		return &StepOutcome{}, nil
	}}
	steps := []Step{pass(types.StepReview), pass(types.StepTest), pass(types.StepDocument), pass(types.StepLint), pass(types.StepPush), pass(types.StepPR), ci}
	recovered := NewExecutor(database, p, nil, nil, steps, nil)
	recoveredRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- recovered.Resume(context.Background(), recoveredRun, repo, workDir) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recovered.mu.Lock()
		waiting := recovered.waiting && recovered.waitingRoundID == secondRound.ID
		recovered.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Resume did not reconstruct the protected-path retry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := recovered.RespondWithOverrides(types.StepCI, types.ActionFix, []string{"protected-path-refusal"}, nil, nil, nil, ""); err != nil {
		t.Fatalf("recovered protected-path retry: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovered protected-path retry did not finish")
	}
	want := []types.StepName{types.StepCI, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI}
	if !slices.Equal(order, want) {
		t.Fatalf("execution order = %v, want %v", order, want)
	}
}

func stringPointer(value string) *string { return &value }

func TestExecutor_BoundCIResponseRefusesAStaleRoundAndLeavesTheGateParked(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://example.invalid/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	for _, step := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		result, err := database.InsertStepResult(run.ID, step)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateStepStatus(result.ID, types.StepStatusCompleted); err != nil {
			t.Fatal(err)
		}
	}
	ciResult, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	round, err := database.InsertStepRound(ciResult.ID, 1, "initial", stringPointer(boundCIGateFindings), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, ciResult.ID, types.StepStatusAwaitingApproval, 1, 1, stringPointer(boundCIGateFindings)); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(database, p, nil, nil, nil, nil)
	exec.waiting = true
	exec.waitingStep = types.StepCI
	exec.waitingStepResultID = ciResult.ID
	exec.waitingRoundID = round.ID
	exec.waitingFindings = boundCIGateFindings
	exec.approvalCh = make(chan approvalResponse, 1)
	binding := ResponseRevalidationBinding{
		OperationID: "operation-stale", Fingerprint: "sha256:stale", RunID: run.ID,
		RepoID: repo.ID, Branch: run.Branch, HeadSHA: run.HeadSHA, StepResultID: ciResult.ID, RoundID: "newer-round",
	}
	if _, _, _, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding); err == nil || !strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("stale-round error = %v", err)
	}
	if !exec.waiting {
		t.Fatal("stale request released the parked gate")
	}
	select {
	case <-exec.approvalCh:
		t.Fatal("stale request dispatched a fixer")
	default:
	}
	if operation, err := database.GetBoundResponseOperation(binding.OperationID); err != nil || operation != nil {
		t.Fatalf("stale request recorded operation %#v, error=%v", operation, err)
	}
}
