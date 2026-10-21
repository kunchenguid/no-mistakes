package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var reviewOnlySkips = []types.StepName{types.StepRebase, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI}

type forbiddenScopedStep struct {
	name     types.StepName
	calls    *atomic.Int32
	metadata string
}

func (s *forbiddenScopedStep) Name() types.StepName { return s.name }
func (s *forbiddenScopedStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls.Add(1)
	if s.name == types.StepPR {
		_ = os.WriteFile(s.metadata, []byte("rewritten"), 0600)
	}
	return nil, fmt.Errorf("forbidden stage %s invoked", s.name)
}

// The fixture carries a committed fix and a parked Review through two real
// daemon lifetimes and a database reopen. Future step rows are deliberately
// pending so only the durable policy, not their incidental status, protects it.
func TestScopedReviewPolicySurvivesDaemonReconstruction(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, head := setupTestGitRepo(t, p, d, "scoped-review-recovery")
	r, err := d.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunSkippedSteps(r.ID, reviewOnlySkips); err != nil {
		t.Fatal(err)
	}
	work := p.WorktreeDir(repo.ID, r.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", work, head)
	gitCmd(t, work, "config", "user.name", "Test")
	gitCmd(t, work, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(work, "review-fix.txt"), []byte("preserve this committed fix\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "add", "review-fix.txt")
	gitCmd(t, work, "commit", "-m", "Preserve review fix")
	fixed := gitOutput(t, work, "rev-parse", "HEAD")
	recoveryRef := "refs/no-mistakes/recover/" + r.ID
	gitCmd(t, p.RepoDir(repo.ID), "update-ref", recoveryRef, fixed)
	for _, call := range []func() error{
		func() error { return d.UpdateRunHeadSHA(r.ID, fixed) },
		func() error { return d.UpdateRunPRURL(r.ID, "https://example.com/pull/7") },
		func() error { return d.UpdateRunStatus(r.ID, types.RunRunning) },
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	findings := `{"findings":[],"summary":"approve committed fixes"}`
	for _, name := range types.AllSteps() {
		sr, err := d.InsertStepResult(r.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		switch name {
		case types.StepIntent:
			if err := d.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		case types.StepRebase:
			if err := d.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
				t.Fatal(err)
			}
		case types.StepReview:
			if err := d.StartStep(sr.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.SetStepFindings(sr.ID, findings); err != nil {
				t.Fatal(err)
			}
			if _, err := d.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, fixed, 1); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateStepStatusWithDuration(sr.ID, types.StepStatusAwaitingApproval, 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := d.SetRunAwaitingAgent(r.ID); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(p.Root(), "pr-metadata")
	if err := os.WriteFile(metadata, []byte("original title and body"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	factory := func() []pipeline.Step {
		var stages []pipeline.Step
		for _, name := range types.AllSteps() {
			stages = append(stages, &forbiddenScopedStep{name: name, calls: &calls, metadata: metadata})
		}
		return stages
	}
	start := func(database *db.DB) chan error {
		done := make(chan error, 1)
		go func() { done <- RunWithOptions(p, database, factory) }()
		waitForDaemonReady(t, p)
		return done
	}
	first := start(d)
	stopParkedRunDaemon(t, p, first)
	parked, err := d.GetRun(r.ID)
	if err != nil || parked.Status != types.RunRunning || parked.AwaitingAgentSince == nil {
		t.Fatalf("lost Review park: %+v %v", parked, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	second := start(d)
	defer stopParkedRunDaemon(t, p, second)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var snapshot ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: r.ID}, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Run.SkipPolicyKnown || len(snapshot.Run.SkipSteps) != 7 {
		t.Fatalf("scope absent from snapshot: %+v", snapshot.Run)
	}
	// A response with a stronger requirement must leave this very gate parked.
	mismatch := &ipc.ScopedRespondParams{RespondParams: ipc.RespondParams{RunID: r.ID, Step: types.StepReview, Action: types.ActionFix}, RequiredSkips: []types.StepName{types.StepReview}}
	if err := client.Call(ipc.MethodRespondScoped, mismatch, &ipc.RespondResult{}); err == nil || !strings.Contains(err.Error(), "required skip") {
		t.Fatalf("mismatch accepted: %v", err)
	}
	mismatch.Action = types.ActionApprove
	mismatch.RequiredSkips = reviewOnlySkips
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = client.Call(ipc.MethodRespondScoped, mismatch, &ipc.RespondResult{})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scoped approval: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	completed := waitForRunTerminalState(t, d, r.ID)
	if completed.Status != types.RunCompleted || completed.HeadSHA != fixed || completed.PRURL == nil || *completed.PRURL != "https://example.com/pull/7" {
		t.Fatalf("lost committed fix or PR: %+v", completed)
	}
	if calls.Load() != 0 {
		t.Fatalf("forbidden executions (including PR rewrite or CI polling): %d", calls.Load())
	}
	if got := gitOutput(t, p.RepoDir(repo.ID), "rev-parse", recoveryRef); got != fixed {
		t.Fatalf("recovery anchor=%s want %s", got, fixed)
	}
	content, err := os.ReadFile(metadata)
	if err != nil || string(content) != "original title and body" {
		t.Fatalf("PR metadata changed: %q %v", content, err)
	}
	rows, err := d.GetStepsByRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName != types.StepReview && row.StepName != types.StepIntent && row.Status != types.StepStatusSkipped {
			t.Fatalf("%s was not skipped", row.StepName)
		}
	}
}
