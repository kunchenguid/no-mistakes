package db

import (
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type boundResponseFixture struct {
	database *DB
	repo     *Repo
	run      *Run
	ci       *StepResult
	round    *StepRound
	decision BoundResponseDecision
}

func seedBoundResponseFixture(t *testing.T) *boundResponseFixture {
	t.Helper()
	database := openTestDB(t)
	repo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
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
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-1","severity":"error","description":"repair CI"}]}`
	round, err := database.InsertStepRound(ci.ID, 1, "initial", &findings, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPublication(run.ID, PushBinding{HeadSHA: "head", TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://example.invalid/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, ci.ID, types.StepStatusAwaitingApproval, 1, 10, &findings); err != nil {
		t.Fatal(err)
	}
	decision := BoundResponseDecision{
		OperationID:      "operation-1",
		Fingerprint:      "sha256:request-1",
		RunID:            run.ID,
		RepoID:           repo.ID,
		Branch:           run.Branch,
		HeadSHA:          run.HeadSHA,
		StepResultID:     ci.ID,
		RoundID:          round.ID,
		SelectedIDsJSON:  `["ci-1"]`,
		UserFindingsJSON: findings,
		DispositionsJSON: `{"fixed":["ci-1"]}`,
	}
	return &boundResponseFixture{database: database, repo: repo, run: run, ci: ci, round: round, decision: decision}
}

func TestRecordBoundResponseDecisionBindsTheExactPublishedCIGate(t *testing.T) {
	f := seedBoundResponseFixture(t)
	accepted, err := f.database.RecordBoundResponseDecision(f.decision)
	if err != nil || !accepted {
		t.Fatalf("accepted = %v, error = %v", accepted, err)
	}
	op, err := f.database.GetBoundResponseOperation(f.decision.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if op == nil || op.RunID != f.run.ID || op.RepoID != f.repo.ID || op.Branch != f.run.Branch || op.HeadSHA != f.run.HeadSHA || op.StepResultID != f.ci.ID || op.RoundID != f.round.ID || op.AcceptedAt == 0 {
		t.Fatalf("operation = %#v, want the exact run, gate, round, head, and full-revalidation demand", op)
	}
	if op.Fingerprint != f.decision.Fingerprint || op.DispositionsJSON != f.decision.DispositionsJSON {
		t.Fatalf("operation lost its request identity or receipt: %#v", op)
	}
	rounds, err := f.database.GetRoundsByStep(f.ci.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds = %#v, error = %v", rounds, err)
	}
	if rounds[0].SelectedFindingIDs == nil || *rounds[0].SelectedFindingIDs != f.decision.SelectedIDsJSON || rounds[0].UserFindingsJSON == nil || *rounds[0].UserFindingsJSON != f.decision.UserFindingsJSON {
		t.Fatalf("accepted operation and fixer input were not recorded atomically: %#v", rounds[0])
	}

	accepted, err = f.database.RecordBoundResponseDecision(f.decision)
	if err != nil || !accepted {
		t.Fatalf("identical durable retry accepted = %v, error = %v", accepted, err)
	}
	replayed, err := f.database.GetBoundResponseOperation(f.decision.OperationID)
	if err != nil || replayed.AcceptedAt != op.AcceptedAt {
		t.Fatalf("identical retry changed its receipt: before=%#v after=%#v error=%v", op, replayed, err)
	}
}

func TestRecordBoundResponseDecisionRefusesStaleOrIneligibleEvidenceWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *boundResponseFixture)
	}{
		{name: "wrong repo", mutate: func(_ *testing.T, f *boundResponseFixture) { f.decision.RepoID = "repo-other" }},
		{name: "wrong branch", mutate: func(_ *testing.T, f *boundResponseFixture) { f.decision.Branch = "other" }},
		{name: "stale head", mutate: func(_ *testing.T, f *boundResponseFixture) { f.decision.HeadSHA = "head-other" }},
		{name: "wrong result", mutate: func(_ *testing.T, f *boundResponseFixture) { f.decision.StepResultID = "step-other" }},
		{name: "wrong round", mutate: func(_ *testing.T, f *boundResponseFixture) { f.decision.RoundID = "round-other" }},
		{name: "not active", mutate: func(t *testing.T, f *boundResponseFixture) {
			if err := f.database.UpdateRunStatus(f.run.ID, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "not parked", mutate: func(t *testing.T, f *boundResponseFixture) {
			if err := f.database.ClearRunAwaitingAgent(f.run.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "not published", mutate: func(t *testing.T, f *boundResponseFixture) {
			if _, err := f.database.sql.Exec(`UPDATE runs SET pr_url = NULL WHERE id = ?`, f.run.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "current head is not published", mutate: func(t *testing.T, f *boundResponseFixture) {
			if err := f.database.UpdateRunHeadSHAForRevalidation(f.run.ID, "unpublished-head"); err != nil {
				t.Fatal(err)
			}
			f.decision.HeadSHA = "unpublished-head"
		}},
		{name: "required step skipped", mutate: func(t *testing.T, f *boundResponseFixture) {
			steps, err := f.database.GetStepsByRun(f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range steps {
				if step.StepName == types.StepTest {
					if err := f.database.UpdateStepStatus(step.ID, types.StepStatusSkipped); err != nil {
						t.Fatal(err)
					}
				}
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := seedBoundResponseFixture(t)
			tc.mutate(t, f)
			accepted, err := f.database.RecordBoundResponseDecision(f.decision)
			if err != nil {
				t.Fatal(err)
			}
			if accepted {
				t.Fatal("stale or ineligible evidence was accepted")
			}
			op, err := f.database.GetBoundResponseOperation(f.decision.OperationID)
			if err != nil || op != nil {
				t.Fatalf("refused request left operation %#v, error = %v", op, err)
			}
			rounds, err := f.database.GetRoundsByStep(f.ci.ID)
			if err != nil || len(rounds) != 1 {
				t.Fatalf("rounds = %#v, error = %v", rounds, err)
			}
			if rounds[0].SelectedFindingIDs != nil || rounds[0].UserFindingsJSON != nil {
				t.Fatalf("refusal partially mutated the round: %#v", rounds[0])
			}
		})
	}
}

func TestRecordBoundResponseDecisionAllowsOnlyOneConcurrentOperation(t *testing.T) {
	f := seedBoundResponseFixture(t)
	second := f.decision
	second.OperationID = "operation-2"
	second.Fingerprint = "sha256:request-2"

	type result struct {
		accepted bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, decision := range []BoundResponseDecision{f.decision, second} {
		decision := decision
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			accepted, err := f.database.RecordBoundResponseDecision(decision)
			results <- result{accepted: accepted, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	accepted := 0
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.accepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted operations = %d, want exactly one", accepted)
	}
	one, err := f.database.GetBoundResponseOperation(f.decision.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	two, err := f.database.GetBoundResponseOperation(second.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if (one == nil) == (two == nil) {
		t.Fatalf("durable operations one=%#v two=%#v, want exactly one", one, two)
	}
}
