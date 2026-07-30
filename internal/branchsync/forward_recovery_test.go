package branchsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type forwardRecoverFixture struct {
	*recoverFixture
	recorded  string
	candidate string
}

func newForwardRecoverFixture(t *testing.T) *forwardRecoverFixture {
	t.Helper()
	f := newRecoverFixture(t, types.RunCompleted)
	recorded := mustRun(t, f.gate, "rev-parse", f.preserved+"^")
	if recorded == f.submitted || recorded == f.preserved {
		t.Fatal("fixture must have submitted < recorded < candidate")
	}
	if err := f.db.UpdateRunHeadSHA(f.run.ID, recorded); err != nil {
		t.Fatal(err)
	}
	for _, name := range types.AllSteps() {
		step, err := f.db.InsertStepResult(f.run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name.Order() <= types.StepLint.Order() {
			if err := f.db.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.db.CompleteStep(step.ID, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		} else if err := f.db.CompleteStepWithStatus(step.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	f.run, _ = f.db.GetRun(f.run.ID)
	return &forwardRecoverFixture{recoverFixture: f, recorded: recorded, candidate: f.preserved}
}

func (f *forwardRecoverFixture) recover() ForwardRecoveryResult {
	f.t.Helper()
	return f.service.RecoverAuthorizedForwardHead(f.ctx, f.run.ID, f.candidate)
}

func (f *forwardRecoverFixture) candidateAnchor() string {
	return recoveryCandidateAnchorRef(f.run.ID, f.candidate)
}

func TestAuthorizedForwardRecoveryExactIncident(t *testing.T) {
	f := newForwardRecoverFixture(t)
	result := f.recover()
	if !result.Recovered || !result.Changed || result.Phase != ForwardRecoveryPhaseComplete || result.State != StateCustodyReturned {
		t.Fatalf("recovery = %#v", result)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.candidate {
		t.Fatalf("local HEAD = %s, want %s", got, f.candidate)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.candidate {
		t.Fatalf("gate moved to %s", got)
	}
	if got := mustRun(t, f.local, "rev-parse", f.candidateAnchor()); got != f.candidate {
		t.Fatalf("anchor = %s, want %s", got, f.candidate)
	}
	run, _ := f.db.GetRun(f.run.ID)
	if run.HeadSHA != f.candidate || run.CustodyReturnedAt == nil {
		t.Fatalf("run after recovery = %#v", run)
	}
	audit, err := f.db.GetRunHeadRecovery(f.run.ID)
	if err != nil || audit == nil || audit.ExpectedHeadSHA != f.recorded || audit.CandidateHeadSHA != f.candidate || audit.LocalHeadSHA != f.submitted || audit.AnchorRef != f.candidateAnchor() {
		t.Fatalf("audit = %#v, %v", audit, err)
	}

	second := f.recover()
	if !second.Recovered || second.Changed || second.Phase != ForwardRecoveryPhaseComplete {
		t.Fatalf("idempotent retry = %#v", second)
	}
}

func TestAuthorizedForwardRecoveryRejectsCandidateSyntaxAndType(t *testing.T) {
	f := newForwardRecoverFixture(t)
	tree := mustRun(t, f.gate, "rev-parse", f.candidate+"^{tree}")
	for name, candidate := range map[string]string{
		"abbreviation": f.candidate[:12],
		"ref":          "refs/heads/feature/recover",
		"revspec":      f.candidate + "^",
		"uppercase":    strings.ToUpper(f.candidate),
		"zero":         strings.Repeat("0", 40),
		"tree":         tree,
		"unknown":      strings.Repeat("1", 40),
	} {
		t.Run(name, func(t *testing.T) {
			result := f.service.RecoverAuthorizedForwardHead(f.ctx, f.run.ID, candidate)
			if result.Recovered {
				t.Fatalf("candidate %q recovered", candidate)
			}
			f.assertUnadopted()
		})
	}
}

func TestAuthorizedForwardRecoveryRunAndPublicationDenials(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*forwardRecoverFixture)
	}{
		{"pending", func(f *forwardRecoverFixture) { mustDB(t, f.db.UpdateRunStatus(f.run.ID, types.RunPending)) }},
		{"running", func(f *forwardRecoverFixture) { mustDB(t, f.db.UpdateRunStatus(f.run.ID, types.RunRunning)) }},
		{"failed", func(f *forwardRecoverFixture) { mustDB(t, f.db.UpdateRunStatus(f.run.ID, types.RunFailed)) }},
		{"cancelled", func(f *forwardRecoverFixture) { mustDB(t, f.db.UpdateRunStatus(f.run.ID, types.RunCancelled)) }},
		{"run error", func(f *forwardRecoverFixture) { mustDB(t, f.db.UpdateRunError(f.run.ID, "incident error")) }},
		{"awaiting agent", func(f *forwardRecoverFixture) { mustDB(t, f.db.SetRunAwaitingAgent(f.run.ID)) }},
		{"push active", func(f *forwardRecoverFixture) { mustDB(t, f.db.SetRunPushActive(f.run.ID, true)) }},
		{"push binding", func(f *forwardRecoverFixture) {
			mustDB(t, f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.recorded, TargetKind: "upstream", TargetFingerprint: "x", Ref: "refs/heads/feature/recover"}))
		}},
		{"pr authority", func(f *forwardRecoverFixture) {
			mustDB(t, f.db.UpdateRunPRURL(f.run.ID, "https://example.invalid/pr/1"))
		}},
		{"ci authority", func(f *forwardRecoverFixture) { mustDB(t, f.db.SetRunCIReady(f.run.ID, true)) }},
		{"newer same branch", func(f *forwardRecoverFixture) {
			_, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
			mustDB(t, err)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newForwardRecoverFixture(t)
			tt.mutate(f)
			if result := f.recover(); result.Recovered {
				t.Fatalf("recovery unexpectedly succeeded: %#v", result)
			}
			f.assertUnadopted()
		})
	}
}

func TestAuthorizedForwardRecoveryRejectsCustodyStampWithoutAudit(t *testing.T) {
	f := newForwardRecoverFixture(t)
	mustDB(t, f.db.SetRunCustodyReturned(f.run.ID))
	result := f.recover()
	if result.Recovered {
		t.Fatalf("recovery unexpectedly succeeded: %#v", result)
	}
	run, _ := f.db.GetRun(f.run.ID)
	if run.HeadSHA != f.recorded || run.CustodyReturnedAt == nil {
		t.Fatalf("pre-existing custody state changed: %#v", run)
	}
	if audit, _ := f.db.GetRunHeadRecovery(f.run.ID); audit != nil {
		t.Fatalf("unexpected audit: %#v", audit)
	}
}

func TestAuthorizedForwardRecoveryStepMatrixDenials(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*forwardRecoverFixture)
	}{
		{"duplicate", func(f *forwardRecoverFixture) {
			_, err := f.db.InsertStepResult(f.run.ID, types.StepIntent)
			mustDB(t, err)
		}},
		{"validation skipped", func(f *forwardRecoverFixture) {
			step := exactStep(t, f, types.StepReview)
			mustDB(t, f.db.UpdateStepStatus(step.ID, types.StepStatusSkipped))
		}},
		{"validation failed", func(f *forwardRecoverFixture) {
			step := exactStep(t, f, types.StepTest)
			mustDB(t, f.db.UpdateStepStatus(step.ID, types.StepStatusFailed))
		}},
		{"publication completed", func(f *forwardRecoverFixture) {
			step := exactStep(t, f, types.StepPush)
			mustDB(t, f.db.UpdateStepStatus(step.ID, types.StepStatusCompleted))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newForwardRecoverFixture(t)
			tt.mutate(f)
			if result := f.recover(); result.Recovered {
				t.Fatalf("recovery unexpectedly succeeded: %#v", result)
			}
			f.assertUnadopted()
		})
	}
}

func TestAuthorizedForwardRecoveryWorktreeDenials(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		mustWrite(t, filepath.Join(f.local, "dirty.txt"), "dirty\n")
		if result := f.recover(); result.Recovered || result.Safety != "blocked_forward_worktree" {
			t.Fatalf("dirty result = %#v", result)
		}
		f.assertUnadopted()
	})
	t.Run("sequencer", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		gitPath := mustRun(t, f.local, "rev-parse", "--git-path", "sequencer")
		if !filepath.IsAbs(gitPath) {
			gitPath = filepath.Join(f.local, gitPath)
		}
		if err := os.MkdirAll(gitPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if result := f.recover(); result.Recovered {
			t.Fatalf("sequencer result = %#v", result)
		}
		f.assertUnadopted()
	})
	t.Run("wrong branch", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		mustRun(t, f.local, "checkout", "-b", "other", f.submitted)
		if result := f.recover(); result.Recovered {
			t.Fatalf("wrong branch result = %#v", result)
		}
		f.assertUnadopted()
	})
	t.Run("detached", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		mustRun(t, f.local, "checkout", "--detach", f.submitted)
		if result := f.recover(); result.Recovered {
			t.Fatalf("detached result = %#v", result)
		}
		f.assertUnadopted()
	})
	t.Run("duplicate checkout", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		duplicate := filepath.Join(t.TempDir(), "duplicate")
		mustRun(t, f.local, "worktree", "add", "--force", duplicate, f.run.Branch)
		if result := f.recover(); result.Recovered {
			t.Fatalf("duplicate result = %#v", result)
		}
		f.assertUnadopted()
	})
}

func TestAuthorizedForwardRecoveryAncestryAndGateDenials(t *testing.T) {
	t.Run("gate mismatch", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.recorded, f.candidate)
		if result := f.recover(); result.Recovered {
			t.Fatalf("gate mismatch result = %#v", result)
		}
		f.assertUnadopted()
	})
	t.Run("non ancestor candidate", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		tree := mustRun(t, f.gate, "rev-parse", f.submitted+"^{tree}")
		divergent := mustRun(t, f.gate, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit-tree", tree, "-p", f.submitted, "-m", "divergent candidate")
		mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", divergent, f.candidate)
		f.candidate = divergent
		result := f.recover()
		if result.Recovered || result.Safety != "blocked_forward_candidate_ancestry" {
			t.Fatalf("non-ancestor result = %#v", result)
		}
		f.assertUnadopted()
		if got := mustRun(t, f.local, "rev-parse", f.candidateAnchor()); got != divergent {
			t.Fatalf("honest late-refusal anchor = %s", got)
		}
	})
}

func TestAuthorizedForwardRecoveryCASRacesAndMonotonicRetries(t *testing.T) {
	t.Run("DB head CAS loss before mutation", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		f.service.forwardRecoveryHooks.BeforeHeadCAS = func() error {
			return f.db.UpdateRunStatus(f.run.ID, types.RunCancelled)
		}
		result := f.recover()
		if result.Recovered || result.Safety != "blocked_forward_head_cas" {
			t.Fatalf("head CAS loss = %#v", result)
		}
		f.assertUnadopted()
		if got := mustRun(t, f.local, "rev-parse", f.candidateAnchor()); got != f.candidate {
			t.Fatalf("anchor missing after late CAS refusal: %s", got)
		}
	})

	for _, boundary := range []string{"anchor", "head_cas", "fast_forward", "custody_cas"} {
		t.Run("retry after "+boundary, func(t *testing.T) {
			f := newForwardRecoverFixture(t)
			crash := errors.New("simulated crash")
			switch boundary {
			case "anchor":
				f.service.forwardRecoveryHooks.AfterAnchor = func() error { return crash }
			case "head_cas":
				f.service.forwardRecoveryHooks.AfterHeadCAS = func() error { return crash }
			case "fast_forward":
				f.service.forwardRecoveryHooks.AfterFastForward = func() error { return crash }
			case "custody_cas":
				f.service.forwardRecoveryHooks.BeforeCustodyCAS = func() error { return crash }
			}
			first := f.recover()
			if first.Recovered || !strings.Contains(first.Error, "simulated crash") {
				t.Fatalf("first result = %#v", first)
			}
			if !first.Changed {
				t.Fatalf("durable %s boundary was not reported as changed: %#v", boundary, first)
			}
			f.service.forwardRecoveryHooks = forwardRecoveryHooks{}
			second := f.recover()
			if !second.Recovered || second.Phase != ForwardRecoveryPhaseComplete {
				t.Fatalf("retry = %#v", second)
			}
		})
	}
}

func (f *forwardRecoverFixture) assertUnadopted() {
	f.t.Helper()
	if got := mustRun(f.t, f.local, "rev-parse", f.run.Branch); got != f.submitted {
		f.t.Fatalf("local branch = %s, want submitted %s", got, f.submitted)
	}
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run == nil {
		f.t.Fatalf("reload run: %#v, %v", run, err)
	}
	if run.HeadSHA != f.recorded || run.CustodyReturnedAt != nil {
		f.t.Fatalf("run mutated: %#v", run)
	}
	audit, err := f.db.GetRunHeadRecovery(f.run.ID)
	if err != nil || audit != nil {
		f.t.Fatalf("unexpected audit: %#v, %v", audit, err)
	}
}

func exactStep(t *testing.T, f *forwardRecoverFixture, name types.StepName) *db.StepResult {
	t.Helper()
	steps, err := f.db.GetStepsByRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == name {
			return step
		}
	}
	t.Fatalf("step %s missing", name)
	return nil
}

func mustDB(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
