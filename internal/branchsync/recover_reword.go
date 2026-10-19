package branchsync

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RecoverReworded accepts an operator-named rewrite left by a failed pre-push
// Review round. It never discovers or chooses dangling objects. The submitted
// head must still be the clean caller and gate head; every tree after the
// run's recorded post-rebase head must match. Both histories are retained
// before ordinary recovery takes over.
func (s *Service) RecoverReworded(ctx context.Context, recorded, rewritten string) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	state, run, _ := s.inspect(ctx)
	refuse := func(message string) State {
		return blockedPlan(state, state.State, "blocked_recover_reword_unproven", message)
	}
	if run == nil || state.State != StatePipelineOwned || run.Status != types.RunFailed ||
		run.HeadSHA != recorded || run.SubmittedHeadSHA == nil || run.TerminalHeadVerifiedAt != nil ||
		run.CustodyReturnedAt != nil || run.ReviewApprovedHeadSHA != nil ||
		(run.PushGeneration != nil && *run.PushGeneration != 0) ||
		(run.LastPushedSHA != nil && *run.LastPushedSHA != "") {
		return refuse("reword recovery requires the exact observed head of a failed, unpublished, unverified Review run; no files or refs were changed")
	}
	if run.RebasedHeadSHA == nil {
		return refuse("this run recorded no post-rebase head to anchor the reword proof; inspect the Review log and reconcile manually; no files or refs were changed")
	}
	submitted, rebased := *run.SubmittedHeadSHA, *run.RebasedHeadSHA
	if !state.Local.Clean || state.Local.Head != submitted || strings.TrimSpace(s.GateDir) == "" {
		return refuse("reword recovery requires the clean caller at the submitted head and an available local gate; no files or refs were changed")
	}
	branchRef := "refs/heads/" + state.Local.Branch
	if symbolic, err := git.Run(ctx, s.GateDir, "symbolic-ref", "-q", branchRef); err == nil && symbolic != "" {
		return refuse("the gate branch is symbolic; no files or refs were changed")
	}
	gateHead, exists, err := git.ExactRefTarget(ctx, s.GateDir, branchRef)
	if err != nil || !exists || gateHead != submitted {
		return refuse("the gate branch no longer equals the submitted head; no files or refs were changed")
	}
	if err := custody.VerifyReword(ctx, s.GateDir, rebased, recorded, rewritten); err != nil {
		return refuse(err.Error() + "; no files or refs were changed")
	}
	// Preflight all anchors before writing any of them. The transaction also
	// verifies the gate lane; a moved or symbolic ref never gets overwritten.
	transaction := "start\nverify " + branchRef + " " + submitted + "\n"
	anchors := []struct{ ref, head string }{
		{custody.RewordRef(run.ID, recorded), recorded},
		{custody.RewordRef(run.ID, rewritten), rewritten},
		{custody.RecoveryRef(run.ID), rewritten},
	}
	for _, anchor := range anchors {
		for _, dir := range []string{s.GateDir, s.workDir()} {
			if symbolic, err := git.Run(ctx, dir, "symbolic-ref", "-q", anchor.ref); err == nil && symbolic != "" {
				return refuse("a recovery anchor is symbolic; no files or refs were changed")
			}
			head, exists, err := git.ExactRefTarget(ctx, dir, anchor.ref)
			if err != nil || (exists && head != anchor.head) {
				return refuse("a recovery anchor conflicts; no files or refs were changed")
			}
			if dir == s.GateDir {
				verb := "create"
				if exists {
					verb = "verify"
				}
				transaction += fmt.Sprintf("%s %s %s\n", verb, anchor.ref, anchor.head)
			}
		}
	}
	if s.beforeRecoverTerminalHeadPreserve != nil {
		s.beforeRecoverTerminalHeadPreserve()
	}
	current, currentRun, _ := s.inspect(ctx)
	if currentRun == nil || currentRun.ID != run.ID || currentRun.HeadSHA != recorded ||
		current.Local.Head != submitted || current.Local.Branch != state.Local.Branch || !current.Local.Clean {
		return refuse("the caller or owning run changed during the reword proof; no files or refs were changed")
	}
	if _, err := git.RunWithInput(ctx, s.GateDir, transaction+"prepare\ncommit\n", "update-ref", "--stdin", "--no-deref"); err != nil {
		return refuse("the gate or recovery anchors changed during the reword proof; no files or refs were changed")
	}
	if err := custody.RemapRewordedRange(ctx, s.DB, s.GateDir, s.Repo.ID, run.Branch, run.ID, rebased, recorded, rewritten); err != nil {
		return refuse("the uncertified fixer range could not be carried across the reword (" + err.Error() + "); both histories remain anchored and custody was not returned")
	}
	updated, err := s.DB.VerifyTerminalReword(run.ID, submitted, rebased, recorded, rewritten)
	if err != nil || !updated {
		return refuse("the run changed during the reword proof; both histories remain anchored and custody was not returned")
	}
	// No validation is inherited: ordinary recovery returns custody only. A
	// fresh run must independently review and test the selected history.
	return s.Recover(ctx, false)
}
