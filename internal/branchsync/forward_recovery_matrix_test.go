package branchsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExactForwardRecoveryAuthorityAndObjectDenialMatrix(t *testing.T) {
	t.Run("run ID syntax and exact lookup", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		for _, runID := range []string{
			f.run.ID[:12], strings.ToLower(f.run.ID), "01KYMMY0K86DVCWW2ERM6JKBKI", strings.Repeat("0", 26),
		} {
			result := f.service.RecoverAuthorizedForwardHead(f.ctx, runID, f.candidate)
			if result.Recovered {
				t.Fatalf("run ID %q recovered: %#v", runID, result)
			}
			f.assertUnadopted()
		}
		missing := "01KYMMY0K86DVCWW2ERM6JKBKY"
		if missing == f.run.ID {
			missing = "01KYMMY0K86DVCWW2ERM6JKBKZ"
		}
		if result := f.service.RecoverAuthorizedForwardHead(f.ctx, missing, f.candidate); result.Recovered || result.Safety != "blocked_forward_run_ineligible" {
			t.Fatalf("missing exact run = %#v", result)
		}
		f.assertUnadopted()
	})

	t.Run("blob candidate", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		blob := mustRun(t, f.gate, "rev-parse", f.candidate+":fix.txt")
		if result := f.service.RecoverAuthorizedForwardHead(f.ctx, f.run.ID, blob); result.Recovered {
			t.Fatalf("blob recovered: %#v", result)
		}
		f.assertUnadopted()
	})

	t.Run("wrong registered repo", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		other, err := f.db.InsertRepo(t.TempDir(), "https://example.invalid/other.git", "main")
		if err != nil {
			t.Fatal(err)
		}
		f.service.Repo = other
		if result := f.recover(); result.Recovered || result.Safety != "blocked_forward_wrong_repo" {
			t.Fatalf("wrong repo = %#v", result)
		}
		f.assertUnadopted()
	})

	t.Run("wrong worktree common directory", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		other := filepath.Join(t.TempDir(), "other")
		mustRun(t, t.TempDir(), "init", "-b", f.run.Branch, other)
		configureIdentity(t, other)
		mustWrite(t, filepath.Join(other, "file"), "other\n")
		mustRun(t, other, "add", "file")
		mustRun(t, other, "commit", "-m", "other")
		f.service.WorkDir = other
		if result := f.recover(); result.Recovered || result.Safety != "blocked_forward_worktree" {
			t.Fatalf("wrong common directory = %#v", result)
		}
		f.assertUnadopted()
	})

	for _, tt := range []struct {
		name   string
		mutate func(*forwardRecoverFixture)
	}{
		{"missing gate", func(f *forwardRecoverFixture) { f.service.GateDir = filepath.Join(f.t.TempDir(), "missing.git") }},
		{"missing gate branch", func(f *forwardRecoverFixture) { mustRun(f.t, f.gate, "update-ref", "-d", "refs/heads/"+f.run.Branch) }},
		{"local submitted mismatch", func(f *forwardRecoverFixture) { mustRun(f.t, f.local, "reset", "--hard", f.base) }},
		{"candidate already recorded without audit", func(f *forwardRecoverFixture) { mustDB(f.t, f.db.UpdateRunHeadSHA(f.run.ID, f.candidate)) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newForwardRecoverFixture(t)
			tt.mutate(f)
			if result := f.recover(); result.Recovered {
				t.Fatalf("denial recovered: %#v", result)
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run.CustodyReturnedAt != nil {
				t.Fatal("denial stamped custody")
			}
			if audit, _ := f.db.GetRunHeadRecovery(f.run.ID); audit != nil {
				t.Fatalf("denial created audit: %#v", audit)
			}
		})
	}

	t.Run("submitted is not ancestor of recorded", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		tree := mustRun(t, f.gate, "rev-parse", f.candidate+"^{tree}")
		recorded := mustRun(t, f.gate, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit-tree", tree, "-m", "unrelated recorded")
		candidate := mustRun(t, f.gate, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit-tree", tree, "-p", recorded, "-m", "candidate")
		mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, candidate, f.candidate)
		mustDB(t, f.db.UpdateRunHeadSHA(f.run.ID, recorded))
		f.recorded, f.candidate = recorded, candidate
		result := f.recover()
		if result.Recovered || result.Safety != "blocked_forward_submitted_ancestry" {
			t.Fatalf("submitted ancestry = %#v", result)
		}
		if got := mustRun(t, f.local, "rev-parse", f.candidateAnchor()); got != candidate {
			t.Fatalf("late-refusal anchor = %s", got)
		}
	})

	t.Run("review authority is not ancestor of candidate", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		tree := mustRun(t, f.gate, "rev-parse", f.submitted+"^{tree}")
		divergent := mustRun(t, f.gate, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit-tree", tree, "-p", f.submitted, "-m", "divergent review")
		mustDB(t, f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, divergent))
		result := f.recover()
		if result.Recovered || result.Safety != "blocked_forward_review_ancestry" {
			t.Fatalf("review ancestry = %#v", result)
		}
		f.assertUnadopted()
	})

	t.Run("conflicting immutable anchor", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		mustRun(t, f.local, "update-ref", f.candidateAnchor(), f.submitted)
		result := f.recover()
		if result.Recovered || result.Safety != "blocked_forward_anchor" {
			t.Fatalf("anchor conflict = %#v", result)
		}
		f.assertUnadopted()
		if got := mustRun(t, f.local, "rev-parse", f.candidateAnchor()); got != f.submitted {
			t.Fatalf("conflicting anchor overwritten with %s", got)
		}
	})
}

func TestExactForwardRecoveryPreCASRaceMatrix(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*forwardRecoverFixture)
	}{
		{"local head", func(f *forwardRecoverFixture) { mustRun(f.t, f.local, "reset", "--hard", f.base) }},
		{"branch", func(f *forwardRecoverFixture) { mustRun(f.t, f.local, "checkout", "-b", "racing-branch", f.submitted) }},
		{"dirty worktree", func(f *forwardRecoverFixture) { mustWrite(f.t, filepath.Join(f.local, "raced.txt"), "race\n") }},
		{"duplicate checkout", func(f *forwardRecoverFixture) {
			mustRun(f.t, f.local, "worktree", "add", "--force", filepath.Join(f.t.TempDir(), "duplicate"), f.run.Branch)
		}},
		{"gate", func(f *forwardRecoverFixture) {
			mustRun(f.t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.recorded, f.candidate)
		}},
		{"run status", func(f *forwardRecoverFixture) { mustDB(f.t, f.db.UpdateRunStatus(f.run.ID, types.RunCancelled)) }},
		{"step", func(f *forwardRecoverFixture) {
			mustDB(f.t, f.db.UpdateStepStatus(exactStep(f.t, f, types.StepTest).ID, types.StepStatusFailed))
		}},
		{"newer row", func(f *forwardRecoverFixture) {
			_, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
			mustDB(f.t, err)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newForwardRecoverFixture(t)
			f.service.forwardRecoveryHooks.AfterAnchor = func() error {
				tt.mutate(f)
				return nil
			}
			result := f.recover()
			if result.Recovered || !strings.Contains(result.Safety, "race") {
				t.Fatalf("pre-CAS race = %#v", result)
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run.HeadSHA != f.recorded || run.CustodyReturnedAt != nil {
				t.Fatalf("pre-CAS race changed authority: %#v", run)
			}
			if audit, _ := f.db.GetRunHeadRecovery(f.run.ID); audit != nil {
				t.Fatalf("pre-CAS race created audit: %#v", audit)
			}
			if got := mustRun(t, f.local, "rev-parse", f.candidateAnchor()); got != f.candidate {
				t.Fatalf("pre-CAS race lost honest anchor: %s", got)
			}
		})
	}
}

func TestExactForwardRecoveryPostCASRaceMatrix(t *testing.T) {
	t.Run("pre-apply dirtiness preserves adopted phase", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		f.service.forwardRecoveryHooks.BeforeFastForward = func() error {
			mustWrite(t, filepath.Join(f.local, "late-dirty.txt"), "dirty\n")
			return nil
		}
		result := f.recover()
		if result.Recovered || result.Safety != "blocked_forward_pre_apply_race" {
			t.Fatalf("pre-apply dirty = %#v", result)
		}
		assertForwardAdoptedOnly(t, f)
	})

	t.Run("pre-apply newer run preserves adopted phase", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		f.service.forwardRecoveryHooks.BeforeFastForward = func() error {
			_, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
			return err
		}
		result := f.recover()
		if result.Recovered || result.Safety != "blocked_forward_pre_apply_race" {
			t.Fatalf("pre-apply newer run = %#v", result)
		}
		assertForwardAdoptedOnly(t, f)
	})

	for _, tt := range []struct {
		name   string
		mutate func(*forwardRecoverFixture)
		safety string
	}{
		{"final dirty", func(f *forwardRecoverFixture) { mustWrite(f.t, filepath.Join(f.local, "final-dirty"), "dirty\n") }, "blocked_forward_final_local"},
		{"final head", func(f *forwardRecoverFixture) { mustRun(f.t, f.local, "reset", "--hard", f.submitted) }, "blocked_forward_final_local"},
		{"final gate", func(f *forwardRecoverFixture) {
			mustRun(f.t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.recorded, f.candidate)
		}, "blocked_forward_final_gate"},
		{"final anchor", func(f *forwardRecoverFixture) {
			mustRun(f.t, f.local, "update-ref", "-d", f.candidateAnchor(), f.candidate)
		}, "blocked_forward_final_anchor"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newForwardRecoverFixture(t)
			f.service.forwardRecoveryHooks.AfterFastForward = func() error {
				tt.mutate(f)
				return nil
			}
			result := f.recover()
			if result.Recovered || result.Safety != tt.safety {
				t.Fatalf("final race = %#v", result)
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run.HeadSHA != f.candidate || run.CustodyReturnedAt != nil {
				t.Fatalf("final race state = %#v", run)
			}
			if audit, _ := f.db.GetRunHeadRecovery(f.run.ID); audit == nil || audit.CandidateHeadSHA != f.candidate {
				t.Fatalf("final race audit = %#v", audit)
			}
		})
	}

	for _, tt := range []struct {
		name   string
		mutate func(*forwardRecoverFixture) error
	}{
		{"run publication", func(f *forwardRecoverFixture) error {
			return f.db.UpdateRunPRURL(f.run.ID, "https://example.invalid/pr/1")
		}},
		{"step", func(f *forwardRecoverFixture) error {
			return f.db.UpdateStepStatus(exactStep(f.t, f, types.StepLint).ID, types.StepStatusFailed)
		}},
		{"newer run", func(f *forwardRecoverFixture) error {
			_, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
			return err
		}},
	} {
		t.Run("custody CAS loss "+tt.name, func(t *testing.T) {
			f := newForwardRecoverFixture(t)
			f.service.forwardRecoveryHooks.BeforeCustodyCAS = func() error { return tt.mutate(f) }
			result := f.recover()
			if result.Recovered || result.Safety != "blocked_forward_custody_cas" {
				t.Fatalf("custody race = %#v", result)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.candidate {
				t.Fatalf("custody race local = %s", got)
			}
			run, _ := f.db.GetRun(f.run.ID)
			if run.HeadSHA != f.candidate || run.CustodyReturnedAt != nil {
				t.Fatalf("custody race run = %#v", run)
			}
		})
	}
}

func TestExactForwardRecoveryConflictingRetryDenials(t *testing.T) {
	t.Run("different candidate after adoption", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		crash := errors.New("stop after head CAS")
		f.service.forwardRecoveryHooks.AfterHeadCAS = func() error { return crash }
		if first := f.recover(); !strings.Contains(first.Error, crash.Error()) {
			t.Fatalf("first recovery = %#v", first)
		}
		firstCandidate := f.candidate
		tree := mustRun(t, f.gate, "rev-parse", firstCandidate+"^{tree}")
		secondCandidate := mustRun(t, f.gate, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit-tree", tree, "-p", firstCandidate, "-m", "different candidate")
		mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, secondCandidate, firstCandidate)
		f.candidate = secondCandidate
		f.service.forwardRecoveryHooks = forwardRecoveryHooks{}
		result := f.recover()
		if result.Recovered || result.Safety != "blocked_forward_run_ineligible" {
			t.Fatalf("different candidate retry = %#v", result)
		}
		run, _ := f.db.GetRun(f.run.ID)
		if run.HeadSHA != firstCandidate || run.CustodyReturnedAt != nil {
			t.Fatalf("conflicting retry run = %#v", run)
		}
		audit, _ := f.db.GetRunHeadRecovery(f.run.ID)
		if audit == nil || audit.CandidateHeadSHA != firstCandidate {
			t.Fatalf("conflicting retry audit = %#v", audit)
		}
	})

	t.Run("same candidate with changed review authority", func(t *testing.T) {
		f := newForwardRecoverFixture(t)
		f.service.forwardRecoveryHooks.AfterHeadCAS = func() error { return errors.New("stop") }
		_ = f.recover()
		f.service.forwardRecoveryHooks = forwardRecoveryHooks{}
		// Review authority is audit-bound; changing it must prevent the retry
		// from treating the old audit as authority.
		mustDB(t, f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, f.submitted))
		result := f.recover()
		if result.Recovered || result.Safety != "blocked_forward_run_ineligible" {
			t.Fatalf("changed authority retry = %#v", result)
		}
	})
}

func assertForwardAdoptedOnly(t *testing.T, f *forwardRecoverFixture) {
	t.Helper()
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("partial recovery moved local to %s", got)
	}
	run, _ := f.db.GetRun(f.run.ID)
	if run.HeadSHA != f.candidate || run.CustodyReturnedAt != nil {
		t.Fatalf("partial recovery run = %#v", run)
	}
	audit, err := f.db.GetRunHeadRecovery(f.run.ID)
	if err != nil || audit == nil || audit.CandidateHeadSHA != f.candidate || audit.LocalHeadSHA != f.submitted {
		t.Fatalf("partial recovery audit = %#v, %v", audit, err)
	}
}

func TestExactForwardRecoverySequencerMatrix(t *testing.T) {
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG", "rebase-merge", "rebase-apply", "sequencer"} {
		t.Run(marker, func(t *testing.T) {
			f := newForwardRecoverFixture(t)
			path := mustRun(t, f.local, "rev-parse", "--git-path", marker)
			if !filepath.IsAbs(path) {
				path = filepath.Join(f.local, path)
			}
			if strings.Contains(marker, "rebase-") || marker == "sequencer" {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(f.candidate+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if result := f.recover(); result.Recovered || result.Safety != "blocked_forward_worktree" {
				t.Fatalf("sequencer marker %s = %#v", marker, result)
			}
			f.assertUnadopted()
		})
	}
}
