package branchsync

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// S is submitted, P is published, R is a reviewed rebase whose deliberate fix
// changes S/P content. Ordinary content containment must continue to refuse.
func newReviewedRecoveryFixture(t *testing.T) (*recoverFixture, ReviewedRecoveryRequest) {
	t.Helper()
	f := newRecoverFixture(t, types.RunFailed)
	published := f.preserved
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: published, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover"}); err != nil {
		t.Fatal(err)
	}
	pipeline := filepath.Join(filepath.Dir(f.local), "pipeline")
	mustRun(t, pipeline, "checkout", "--orphan", "reviewed-rebase")
	mustWrite(t, filepath.Join(pipeline, "file.txt"), "reviewed replacement for feature\n")
	mustRun(t, pipeline, "commit", "-am", "reviewed rewrite")
	f.preserved = mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, f.preserved); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	f.run, _ = f.db.GetRun(f.run.ID)
	if err := custody.PreserveRecoveryHead(f.ctx, f.gate, f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	return f, ReviewedRecoveryRequest{RunID: f.run.ID, ExpectedLocalHead: f.submitted, ReviewedHead: f.preserved}
}

func TestReviewedRecoveryAdoptsAlteredReviewedRewriteOnlyWithExactConsent(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	ordinary := f.service.Recover(f.ctx, false)
	if ordinary.Recovered || ordinary.Safety != "blocked_recover_diverged" {
		t.Fatalf("ordinary recovery widened: %#v", ordinary)
	}
	beforeRefs := mustRun(t, f.local, "show-ref")
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Diff, "reviewed replacement for feature") || plan.PublishedHead == "" || plan.Digest == "" {
		t.Fatalf("incomplete preview: %#v", plan)
	}
	if refs := mustRun(t, f.local, "show-ref"); refs != beforeRefs {
		t.Fatal("preview mutated refs")
	}
	if got := f.service.AdoptReviewedRecovery(f.ctx, request, ""); got.Recovered || f.custodyReturned() {
		t.Fatal("implicit consent")
	}
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if !got.Recovered || !got.Changed || !f.custodyReturned() {
		t.Fatalf("adoption: %#v", got)
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != f.preserved {
		t.Fatal("wrong materialized head")
	}
	content, err := os.ReadFile(filepath.Join(f.local, "file.txt"))
	if err != nil || string(content) != "reviewed replacement for feature\n" {
		t.Fatalf("content: %q %v", content, err)
	}
	for _, binding := range reviewedRecoveryAnchors(f.run, request.ExpectedLocalHead) {
		if !exactRawCommitRef(f.ctx, f.local, binding.ref, binding.head) {
			t.Fatalf("lost original object/ref: %#v", binding)
		}
	}
}

func TestReviewedRecoveryAdoptsFromExactPublishedCaller(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	request.ExpectedLocalHead = *f.run.LastPushedSHA
	mustRun(t, f.local, "fetch", f.gate, request.ExpectedLocalHead)
	mustRun(t, f.local, "merge", "--ff-only", request.ExpectedLocalHead)
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if !got.Recovered || mustRun(t, f.local, "rev-parse", "HEAD") != request.ReviewedHead {
		t.Fatalf("published caller adoption: %#v", got)
	}
	for _, binding := range reviewedRecoveryAnchors(f.run, request.ExpectedLocalHead) {
		if !exactRawCommitRef(f.ctx, f.local, binding.ref, binding.head) {
			t.Fatalf("lost published/submitted object: %#v", binding)
		}
	}
}

func TestReviewedRecoveryRetiredPRKeepsCustodyWithoutOfferingPipeline(t *testing.T) {
	t.Parallel()
	for _, prState := range []string{"closed", "merged"} {
		t.Run(prState, func(t *testing.T) {
			f, request := newReviewedRecoveryFixture(t)
			if err := f.db.UpdateRunPRState(f.run.ID, prState); err != nil {
				t.Fatal(err)
			}
			plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
			if !got.Recovered || !got.Changed || !f.custodyReturned() || got.Local.Head != request.ReviewedHead || mustRun(t, f.local, "rev-parse", "HEAD") != request.ReviewedHead {
				t.Fatalf("adoption: %#v", got)
			}
			for _, binding := range reviewedRecoveryAnchors(f.run, request.ExpectedLocalHead) {
				if !exactRawCommitRef(f.ctx, f.local, binding.ref, binding.head) {
					t.Fatalf("lost preserved commit: %#v", binding)
				}
			}
			wantState := StateClosed
			if prState == "merged" {
				wantState = StateMergedRemoteRetained
			}
			if got.State != wantState || got.Safety != "blocked_"+prState || got.PRState != prState || got.NextAction != nil {
				t.Fatalf("retired PR offered follow-up work: %#v", got)
			}
		})
	}
}

func TestReviewedRecoveryRefusesStaleOrUnauthorizedPlansWithoutMovingCaller(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"wrong run", "wrong local", "wrong review", "wrong repository", "wrong branch", "dirty", "untracked", "missing anchor", "symbolic anchor", "unreviewed", "active", "newer terminal", "active race", "caller race", "review race", "anchor race", "gate race", "wrong digest"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, request := newReviewedRecoveryFixture(t)
			plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			consent := plan.Digest
			switch name {
			case "wrong run":
				request.RunID = "wrong"
			case "wrong local":
				request.ExpectedLocalHead = f.base
			case "wrong review":
				request.ReviewedHead = f.submitted
			case "wrong repository":
				repo, err := f.db.InsertRepo(filepath.Join(filepath.Dir(f.local), "foreign"), f.remote, "main")
				if err != nil {
					t.Fatal(err)
				}
				f.service.Repo = repo
			case "wrong branch":
				mustRun(t, f.local, "checkout", "-b", "foreign")
			case "dirty":
				mustWrite(t, filepath.Join(f.local, "file.txt"), "private dirty edit\n")
			case "untracked":
				mustWrite(t, filepath.Join(f.local, "untracked.txt"), "private untracked\n")
			case "missing anchor":
				mustRun(t, f.gate, "update-ref", "-d", f.anchorRef())
			case "symbolic anchor":
				mustRun(t, f.gate, "symbolic-ref", f.anchorRef(), "refs/heads/feature/recover")
			case "unreviewed":
				if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, f.submitted); err != nil {
					t.Fatal(err)
				}
			case "active", "newer terminal":
				other, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
				if err != nil {
					t.Fatal(err)
				}
				if name == "newer terminal" {
					if err := f.db.UpdateRunStatus(other.ID, types.RunCancelled); err != nil {
						t.Fatal(err)
					}
				}
			case "caller race":
				f.service.beforeRecoverBranchMove = func() {
					mustWrite(t, filepath.Join(f.local, "file.txt"), "raced committed edit\n")
					mustRun(t, f.local, "commit", "-am", "caller race")
				}
			case "active race":
				f.service.beforeRecoverBranchMove = func() {
					if _, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base); err != nil {
						t.Fatal(err)
					}
				}
			case "review race":
				f.service.beforeRecoverBranchMove = func() {
					if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, f.submitted); err != nil {
						t.Fatal(err)
					}
				}
			case "anchor race":
				f.service.beforeRecoverBranchMove = func() { mustRun(t, f.gate, "update-ref", f.anchorRef(), f.submitted) }
			case "gate race":
				f.service.beforeRecoverBranchMove = func() { mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted) }
			case "wrong digest":
				consent = strings.Repeat("0", 64)
			}
			before := mustRun(t, f.local, "rev-parse", "HEAD")
			got := f.service.AdoptReviewedRecovery(f.ctx, request, consent)
			if got.Recovered || f.custodyReturned() {
				t.Fatalf("refusal reported success: %#v", got)
			}
			after := mustRun(t, f.local, "rev-parse", "HEAD")
			if name != "caller race" && after != before {
				t.Fatalf("refusal moved caller: %s→%s", before, after)
			}
			if name == "caller race" && after == f.preserved {
				t.Fatal("overwrote raced commit")
			}
		})
	}
}

func TestReviewedRecoveryMaterializationFailurePreservesConcurrentEditsAndCustody(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	f.service.afterRecoverBranchMove = func() { mustWrite(t, filepath.Join(f.local, "file.txt"), "late private edit\n") }
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if got.Recovered || f.custodyReturned() || got.Safety != "blocked_recover_worktree_busy" {
		t.Fatalf("partial outcome: %#v", got)
	}
	content, _ := os.ReadFile(filepath.Join(f.local, "file.txt"))
	if string(content) != "late private edit\n" {
		t.Fatal("destroyed concurrent edit")
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != request.ExpectedLocalHead {
		t.Fatal("branch not restored by CAS")
	}
	if !exactRawCommitRef(f.ctx, f.local, custody.RecoveryLocalRef(f.run.ID), request.ExpectedLocalHead) {
		t.Fatal("lost pre-recovery anchor")
	}
}

func TestReviewedRecoveryLateRunMutationLeavesMaterializedHeadWithoutCustody(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	f.service.afterRecoverBranchMove = func() {
		if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
	}
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if got.Recovered || !got.Changed || f.custodyReturned() || got.Local.Head != request.ReviewedHead || !strings.Contains(got.Error, "materialized") {
		t.Fatalf("false success: %#v", got)
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != request.ReviewedHead {
		t.Fatal("unexpected rollback of materialized result")
	}
}

func TestReviewedRecoveryAlreadyAtReviewedHeadReportsNoHeadChange(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	// The reviewed final head is itself the recorded published commit. The
	// explicit custody action may still be meaningful, but materialization is
	// a no-op and must not claim a branch-head change.
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.preserved, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover"}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.local, "fetch", f.gate, f.preserved)
	mustRun(t, f.local, "checkout", "--detach", f.preserved)
	mustRun(t, f.local, "branch", "-f", f.run.Branch, f.preserved)
	mustRun(t, f.local, "checkout", f.run.Branch)
	request.ExpectedLocalHead = f.preserved
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if !got.Recovered || got.Changed || !f.custodyReturned() || got.Local.Head != f.preserved {
		t.Fatalf("no-op custody receipt: %#v", got)
	}
}

func TestReviewedRecoveryPreviewDisablesExternalDiffAndRefusesReplacedCommits(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	marker := filepath.Join(filepath.Dir(f.local), "external-diff-ran")
	helper := filepath.Join(filepath.Dir(f.local), "external-diff.sh")
	mustWrite(t, helper, "#!/bin/sh\nprintf external > '"+marker+"'\nprintf misleading-diff\n")
	if err := os.Chmod(helper, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.gate, "config", "diff.external", helper)
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil || !strings.Contains(plan.Diff, "reviewed replacement for feature") {
		t.Fatalf("diff: %v %#v", err, plan)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("external diff executed")
	}
	mustRun(t, f.gate, "replace", f.preserved, f.submitted)
	if _, err := f.service.PreviewReviewedRecovery(f.ctx, request); err == nil {
		t.Fatal("replaced commit accepted as exact evidence")
	}
}

func TestReviewedRecoveryRejectsOversizedProofInsteadOfTruncatingConsent(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	pipeline := filepath.Join(filepath.Dir(f.local), "pipeline")
	mustWrite(t, filepath.Join(pipeline, "large.txt"), strings.Repeat("reviewed line\n", 400000))
	mustRun(t, pipeline, "add", "large.txt")
	mustRun(t, pipeline, "commit", "-m", "large reviewed diff")
	request.ReviewedHead = mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, request.ReviewedHead); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, request.ReviewedHead); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.gate, "update-ref", f.anchorRef(), request.ReviewedHead)
	if _, err := f.service.PreviewReviewedRecovery(f.ctx, request); err == nil || !strings.Contains(err.Error(), "4 MiB") {
		t.Fatalf("oversize not refused: %v", err)
	}
	if f.custodyReturned() || mustRun(t, f.local, "rev-parse", "HEAD") != f.submitted {
		t.Fatal("oversized proof mutated caller")
	}
}

func TestReviewedRecoveryRefusesOlderTerminalPushOwnership(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"flag", "running", "fixing"} {
		t.Run(kind, func(t *testing.T) {
			f, request := newReviewedRecoveryFixture(t)
			older := f.run.ID
			latest, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunStatusWithVerifiedHead(latest.ID, types.RunFailed, f.preserved); err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunReviewApprovedHeadSHA(latest.ID, f.preserved); err != nil {
				t.Fatal(err)
			}
			if err := custody.PreserveRecoveryHead(f.ctx, f.gate, latest.ID, f.preserved); err != nil {
				t.Fatal(err)
			}
			request.RunID = latest.ID
			plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "flag" {
				if err := f.db.SetRunPushActive(older, true); err != nil {
					t.Fatal(err)
				}
			} else {
				step, err := f.db.InsertStepResult(older, types.StepPush)
				if err != nil {
					t.Fatal(err)
				}
				status := types.StepStatusRunning
				if kind == "fixing" {
					status = types.StepStatusFixing
				}
				if err := f.db.UpdateStepStatus(step.ID, status); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.PreviewReviewedRecovery(f.ctx, request); err == nil {
				t.Fatal("older unsettled push offered preview")
			}
			got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
			latest, err = f.db.GetRun(latest.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Recovered || latest.CustodyReturnedAt != nil || mustRun(t, f.local, "rev-parse", "HEAD") != f.submitted {
				t.Fatalf("unsettled push adopted: %#v", got)
			}
		})
	}
}

func TestReviewedRecoveryPreviewIncludesSuppressedGitlinkChanges(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	pipeline := filepath.Join(filepath.Dir(f.local), "pipeline")
	mustRun(t, pipeline, "checkout", "--detach", f.submitted)
	mustRun(t, pipeline, "update-index", "--add", "--cacheinfo", "160000,"+f.base+",module")
	mustRun(t, pipeline, "commit", "-m", "submitted gitlink")
	source := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "origin", "HEAD:refs/heads/gitlink-source")
	mustRun(t, f.local, "fetch", f.gate, source)
	mustRun(t, f.local, "reset", "--hard", source)
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: source, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover"}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, pipeline, "checkout", "--detach", f.preserved)
	mustRun(t, pipeline, "update-index", "--add", "--cacheinfo", "160000,"+f.submitted+",module")
	mustRun(t, pipeline, "commit", "-m", "reviewed gitlink")
	target := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, target); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, target); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.gate, "update-ref", f.anchorRef(), target)
	request.ExpectedLocalHead, request.ReviewedHead = source, target
	baseline, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.gate, "config", "diff.ignoreSubmodules", "all")
	mustRun(t, f.gate, "config", "diff.submodule", "log")
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Diff, "-Subproject commit "+f.base) || !strings.Contains(plan.Diff, "+Subproject commit "+f.submitted) || plan.Diff != baseline.Diff || plan.Digest != baseline.Digest {
		t.Fatalf("configuration changed complete gitlink proof: %q", plan.Diff)
	}
}

func TestReviewedRecoveryRequiresEveryPreservedAnchorThroughStamping(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"before move", "after move"} {
		for _, role := range []string{"reviewed", "local", "submitted", "published"} {
			for _, mutation := range []string{"delete", "move", "symbolic"} {
				t.Run(stage+"/"+role+"/"+mutation, func(t *testing.T) {
					t.Parallel()
					f, request := newReviewedRecoveryFixture(t)
					plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
					if err != nil {
						t.Fatal(err)
					}
					refs := map[string]string{
						"reviewed":  custody.RecoveryRef(f.run.ID),
						"local":     custody.RecoveryLocalRef(f.run.ID),
						"submitted": "refs/no-mistakes/recover-submitted/" + f.run.ID,
						"published": "refs/no-mistakes/recover-published/" + f.run.ID,
					}
					mutate := func() {
						ref := refs[role]
						if !strings.Contains(mustRun(t, f.local, "show-ref"), ref) {
							t.Fatal("race ran before preservation")
						}
						switch mutation {
						case "delete":
							mustRun(t, f.local, "update-ref", "-d", ref)
						case "move":
							mustRun(t, f.local, "update-ref", ref, f.base)
						case "symbolic":
							mustRun(t, f.local, "symbolic-ref", ref, "refs/heads/"+f.run.Branch)
						}
					}
					wantHead := request.ExpectedLocalHead
					if stage == "before move" {
						f.service.beforeRecoverBranchMove = mutate
					} else {
						f.service.afterRecoverBranchMove = mutate
						wantHead = request.ReviewedHead
					}
					got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
					if got.Recovered || f.custodyReturned() || got.Local.Head != wantHead || got.Changed != (wantHead != request.ExpectedLocalHead) || mustRun(t, f.local, "rev-parse", "HEAD") != wantHead {
						t.Fatalf("missing or changed preservation accepted: %#v", got)
					}
				})
			}
		}
	}
}

func TestReviewedRecoveryPostCASDetachedHeadReportsActualState(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	originalContent, err := os.ReadFile(filepath.Join(f.local, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f.service.afterRecoverBranchMove = func() {
		mustRun(t, f.local, "checkout", "--detach", request.ReviewedHead)
	}
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if got.Recovered || f.custodyReturned() || !got.Changed || got.Local.Head != request.ReviewedHead || got.Local.Branch != "HEAD" || got.Local.Clean || got.Local.Reason != "dirty" || got.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("dishonest detached outcome: %#v", got)
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != request.ReviewedHead || mustRun(t, f.local, "rev-parse", "--abbrev-ref", "HEAD") != "HEAD" {
		t.Fatal("changed concurrent detached HEAD")
	}
	if mustRun(t, f.local, "rev-parse", "refs/heads/"+f.run.Branch) != request.ExpectedLocalHead {
		t.Fatal("branch rollback did not restore the original head")
	}
	content, err := os.ReadFile(filepath.Join(f.local, "file.txt"))
	if err != nil || string(content) != string(originalContent) {
		t.Fatalf("overwrote the unmaterialized worktree: %q %v", content, err)
	}
	for _, binding := range reviewedRecoveryAnchors(f.run, request.ExpectedLocalHead) {
		if !exactRawCommitRef(f.ctx, f.local, binding.ref, binding.head) {
			t.Fatalf("lost preservation anchor: %#v", binding)
		}
	}
}

func TestReviewedRecoveryFailedMaterializationReportsConcurrentHead(t *testing.T) {
	t.Parallel()
	f, request := newReviewedRecoveryFixture(t)
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var concurrentHead string
	f.service.afterRecoverBranchMove = func() {
		mustWrite(t, filepath.Join(f.local, "file.txt"), "concurrent committed edit\n")
		mustRun(t, f.local, "commit", "-am", "concurrent commit")
		concurrentHead = mustRun(t, f.local, "rev-parse", "HEAD")
		mustWrite(t, filepath.Join(f.local, "file.txt"), "concurrent uncommitted edit\n")
	}
	got := f.service.AdoptReviewedRecovery(f.ctx, request, plan.Digest)
	if got.Recovered || f.custodyReturned() || !got.Changed || got.Local.Head != concurrentHead || got.Local.Clean || got.Safety != "blocked_recover_worktree_busy" || !strings.Contains(got.Error, "could not be restored") {
		t.Fatalf("dishonest partial outcome: %#v", got)
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != concurrentHead {
		t.Fatal("overwrote concurrent commit")
	}
	content, err := os.ReadFile(filepath.Join(f.local, "file.txt"))
	if err != nil || string(content) != "concurrent uncommitted edit\n" {
		t.Fatalf("overwrote concurrent work: %q %v", content, err)
	}
	for _, binding := range reviewedRecoveryAnchors(f.run, request.ExpectedLocalHead) {
		if !exactRawCommitRef(f.ctx, f.local, binding.ref, binding.head) {
			t.Fatalf("lost preservation anchor: %#v", binding)
		}
	}
}

func TestReviewedRecoveryConflictingPreviewAndWrongConsentLeaveObjectsAndRefsUnchanged(t *testing.T) {
	t.Parallel()
	f := newRecoverFixture(t, types.RunFailed)
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: f.preserved, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover"}); err != nil {
		t.Fatal(err)
	}
	pipeline := filepath.Join(filepath.Dir(f.local), "pipeline")
	mustRun(t, pipeline, "checkout", "-B", "reviewed-rewrite", f.base)
	mustWrite(t, filepath.Join(pipeline, "file.txt"), "independently reviewed replacement\n")
	mustRun(t, pipeline, "commit", "-am", "reviewed rewrite")
	reviewed := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, reviewed); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, reviewed); err != nil {
		t.Fatal(err)
	}
	if err := custody.PreserveRecoveryHead(f.ctx, f.gate, f.run.ID, reviewed); err != nil {
		t.Fatal(err)
	}
	request := ReviewedRecoveryRequest{RunID: f.run.ID, ExpectedLocalHead: f.submitted, ReviewedHead: reviewed}
	inventory := func(dir string) map[string][32]byte {
		t.Helper()
		objects := mustRun(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "objects")
		files := make(map[string][32]byte)
		err := filepath.WalkDir(objects, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(objects, path)
			if err != nil {
				return err
			}
			files[relative] = sha256.Sum256(content)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	type snapshot struct {
		objects map[string][32]byte
		refs    string
	}
	before := make(map[string]snapshot)
	for _, dir := range []string{f.local, f.gate} {
		mustRun(t, dir, "gc")
		before[dir] = snapshot{inventory(dir), mustRun(t, dir, "show-ref")}
	}
	assertUnchanged := func() {
		t.Helper()
		for dir, previous := range before {
			if !reflect.DeepEqual(inventory(dir), previous.objects) {
				t.Fatalf("read-only operation changed loose or packed objects in %s", dir)
			}
			if mustRun(t, dir, "show-ref") != previous.refs {
				t.Fatalf("read-only operation changed refs in %s", dir)
			}
		}
		if mustRun(t, f.local, "rev-parse", "HEAD") != f.submitted || f.custodyReturned() {
			t.Fatal("read-only operation moved HEAD or returned custody")
		}
	}
	plan, err := f.service.PreviewReviewedRecovery(f.ctx, request)
	if err != nil || plan.Digest == "" || !strings.Contains(plan.Diff, "independently reviewed replacement") {
		t.Fatalf("conflicting rewrite preview: %#v, %v", plan, err)
	}
	assertUnchanged()
	result := f.service.AdoptReviewedRecovery(f.ctx, request, strings.Repeat("0", 64))
	if result.Recovered || result.Changed || result.Local.Head != f.submitted || !strings.Contains(result.Error, "consent") {
		t.Fatalf("wrong-consent receipt: %#v", result)
	}
	assertUnchanged()
}
