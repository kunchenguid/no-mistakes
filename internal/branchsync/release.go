package branchsync

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// ReleasePublished returns custody without discarding unpublished work. The
// daemon must serialize this with launches on both the custody and publication
// branches and verify the open PR. The run's publication branch must already
// exist at exactly the invoking HEAD on the configured
// push target. Every terminal head and independently moved gate head is
// archived before a compare-and-swap restores the gate lane. Nothing in the
// operator worktree or public history is changed. Missing unpublished objects,
// dirty managed worktrees, symbolic refs and stale generations fail closed.
// A crash after archiving/moving but before stamping is safely retryable.
func (s *Service) ReleasePublished(ctx context.Context, selected, callerHead string, restartOnly bool, verifyPublication func(context.Context) error) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	state, _, _ := s.inspect(ctx)
	fail := func(message string) State {
		return blockedPlan(state, StatePipelineOwned, "blocked_release_published", message)
	}
	if verifyPublication == nil {
		return fail("an existing open PR proof is required; custody was not returned")
	}
	if state.Local.Branch == "" || !state.Local.Clean || state.Local.Head != callerHead {
		return fail("release requires the same clean registered branch and caller HEAD; no custody was returned")
	}
	if bare, err := git.RunBare(ctx, s.GateDir, "rev-parse", "--is-bare-repository"); err != nil || bare != "true" {
		return fail("the registered gate is unavailable; no custody was returned")
	}
	repo, err := s.DB.GetRepo(s.Repo.ID)
	if err != nil || repo == nil {
		return fail("the registered publication target is unavailable")
	}
	runs, err := s.DB.GetRunsByRepo(repo.ID)
	if err != nil {
		return fail("run ownership could not be read")
	}
	var stack []*db.Run
	var latest *db.Run
	for _, r := range runs {
		if r.Branch != state.Local.Branch {
			continue
		}
		if latest == nil {
			latest = r
		}
		if !r.Status.Terminal() || r.PushActive {
			return fail("a live run owns this branch; park or abort it through the supported run lifecycle first")
		}
		if r.CustodyReturnedAt == nil {
			stack = append(stack, r)
		}
	}
	if latest == nil || latest.ID != selected {
		return fail("the selected run is not the current branch generation; inspect status and select its current run")
	}
	if restartOnly && (latest.Status != types.RunFailed || latest.Error == nil || !restartArtifact(*latest.Error)) {
		return fail("reconcile requires a failed run carrying a historical daemon shutdown/restart error")
	}
	branchRef := "refs/heads/" + state.Local.Branch
	publicationRef := "refs/heads/" + latest.PublishBranch()
	if state.Local.Branch == repo.DefaultBranch || latest.PublishBranch() == repo.DefaultBranch {
		return fail("custody release cannot adopt the default branch")
	}
	if latest.PublicationBranch != nil && ptr(latest.PublicationTargetFingerprint) != TargetFingerprint(repo.PushURL()) {
		return fail("the recorded publication target changed; custody was not returned")
	}
	gateHead, exists, err := rawCommitRef(ctx, s.GateDir, branchRef)
	if err != nil {
		return fail("the gate lane is symbolic, non-commit or unreadable; inspect it before recovery")
	}
	pushURL := s.resolvedPushURL(ctx, repo)
	liveCtx, cancel := context.WithTimeout(ctx, s.remoteTimeout())
	live, err := s.runLsRemote(liveCtx, s.workDir(), pushURL, publicationRef)
	cancel()
	if err != nil || live == "" || live != callerHead {
		return fail("the existing publication branch does not exactly match caller HEAD; no unpublished local work may be adopted")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, s.remoteTimeout())
	err = git.FetchRemoteRef(fetchCtx, s.GateDir, pushURL, publicationRef, callerHead)
	cancel()
	if err != nil {
		return fail("the published head could not be verified and imported into the gate")
	}

	// Preflight all sources before adding archives. Dirty managed worktrees can
	// carry uncommitted work that a commit anchor cannot preserve.
	type anchor struct{ runID, head, source string }
	anchors := []anchor{{selected, callerHead, s.GateDir}}
	managedHeads := map[string]string{}
	for _, r := range stack {
		dirs := []string{s.GateDir, s.workDir()}
		if s.Paths != nil || r.WorktreePath() != "" {
			managed := r.WorktreePath()
			if managed == "" {
				managed = worktrees.RecordedDir(s.Paths, r.WorktreePath(), r.RepoID, r.ID)
			}
			if _, err := os.Stat(managed); err == nil {
				root, rootErr := git.FindGitRoot(managed)
				if rootErr != nil || !samePath(root, managed) {
					return fail("a recorded managed worktree cannot be verified")
				}
				if clean, _ := worktreeClean(ctx, managed); !clean {
					return fail("a terminal managed worktree has uncommitted work; commit or preserve it before returning custody")
				}
				managedHead, err := git.HeadSHA(ctx, managed)
				if err != nil {
					return fail("a managed worktree head cannot be verified")
				}
				managedHeads[managed] = managedHead
				anchors = append(anchors, anchor{r.ID, managedHead, managed})
				dirs = append(dirs, managed)
			} else if !os.IsNotExist(err) {
				return fail("a managed worktree could not be inspected")
			}
		}
		for _, head := range []string{r.HeadSHA, ptr(r.SubmittedHeadSHA), ptr(r.LastPushedSHA)} {
			if head == "" {
				continue
			}
			var source string
			for _, dir := range dirs {
				if got, err := git.Run(ctx, dir, "rev-parse", "--verify", head+"^{commit}"); err == nil && got == head {
					source = dir
					break
				}
			}
			if source == "" {
				return fail(fmt.Sprintf("run %s head %s is unavailable; returning custody would leave its work unpreserved", r.ID, head))
			}
			anchors = append(anchors, anchor{r.ID, head, source})
		}
	}
	if exists {
		anchors = append(anchors, anchor{selected, gateHead, s.GateDir})
	}
	for _, a := range anchors {
		ref := releaseArchiveRef(a.runID, a.head)
		if a.source != s.GateDir {
			if _, err := git.RunBare(ctx, s.GateDir, "fetch", "--no-tags", "--no-write-fetch-head", a.source, a.head); err != nil {
				return fail("an owned head could not be imported for archival")
			}
		}
		if err := custody.PreserveRecoveryAnchor(ctx, s.GateDir, ref, a.head); err != nil {
			return fail("an owned head archive conflicts or could not be created; the gate lane was not replaced")
		}
	}
	// Revalidate the provider as well as Git after archive creation.
	if err := verifyPublication(ctx); err != nil {
		return fail("the existing PR changed or became unverifiable during recovery")
	}
	liveCtx, cancel = context.WithTimeout(ctx, s.remoteTimeout())
	live, err = s.runLsRemote(liveCtx, s.workDir(), pushURL, publicationRef)
	cancel()
	if err != nil || live != callerHead {
		return fail("the published branch changed during recovery; retry after inspection")
	}
	freshRepo, err := s.DB.GetRepo(repo.ID)
	if err != nil || freshRepo == nil || freshRepo.UpstreamURL != repo.UpstreamURL || freshRepo.ForkURL != repo.ForkURL || freshRepo.DefaultBranch != repo.DefaultBranch || freshRepo.WorkingPath != repo.WorkingPath {
		return fail("the configured publication target changed during recovery")
	}
	current, currentExists, err := rawCommitRef(ctx, s.GateDir, branchRef)
	if err != nil || currentExists != exists || current != gateHead {
		return fail("the gate lane changed during recovery; its new owner was not replaced")
	}
	old := gateHead
	if !exists {
		old = strings.Repeat("0", len(callerHead))
	}
	for dir, head := range managedHeads {
		current, err := git.HeadSHA(ctx, dir)
		clean, _ := worktreeClean(ctx, dir)
		if err != nil || !clean || current != head {
			return fail("a managed worktree changed during recovery; no custody was returned")
		}
	}
	recheck, _, _ := s.inspect(ctx)
	if recheck.Local != state.Local {
		return fail("the caller branch, head or clean state changed during recovery")
	}
	// Verify every archive under the same ref locks as the gate replacement.
	// A ref deleted, changed or made symbolic since archival aborts the entire
	// transaction. The old gate head never loses its last preservation anchor.
	commands := "start\noption no-deref\n"
	seen := map[string]bool{}
	for _, a := range anchors {
		ref := releaseArchiveRef(a.runID, a.head)
		if !seen[ref] {
			commands += fmt.Sprintf("verify %s %s\n", ref, a.head)
			seen[ref] = true
		}
	}
	archiveChecks := commands
	commands += fmt.Sprintf("update %s %s %s\nprepare\ncommit\n", branchRef, callerHead, old)
	if _, err := git.RunWithInput(ctx, s.GateDir, commands, "update-ref", "--stdin"); err != nil {
		return fail("the gate lane changed while custody was being returned; archived heads remain preserved")
	}
	if err := s.DB.ReleasePublishedCustody(repo, latest, stack); err != nil {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), s.remoteTimeout())
		defer rollbackCancel()
		rollback := archiveChecks
		if exists {
			rollback += fmt.Sprintf("update %s %s %s\n", branchRef, gateHead, callerHead)
		} else {
			rollback += fmt.Sprintf("delete %s %s\n", branchRef, callerHead)
		}
		rollback += "prepare\ncommit\n"
		if _, rollbackErr := git.RunWithInput(rollbackCtx, s.GateDir, rollback, "update-ref", "--stdin"); rollbackErr != nil {
			return fail(fmt.Sprintf("custody generation changed before stamping; gate rollback failed: %v; heads remain archived, inspect before retrying", rollbackErr))
		}
		return fail("custody generation changed before stamping; gate restored and heads remain archived, inspect and retry")
	}
	state.State, state.Safety, state.Error = StateCustodyReturned, "gate_ready", ""
	state.Relation = RelationEqual
	state.Remote = RemoteState{ObservedHead: callerHead, Freshness: "live", ObservedAt: time.Now().Unix()}
	state.Target.Kind, state.Target.Ref = targetKind(repo), publicationRef
	state.Recovery = &RecoveryEvidence{Source: "published_release", RunID: selected, Branch: state.Local.Branch, RequiredHead: callerHead, ArchiveRef: releaseArchiveRef(selected, callerHead), KeepLocal: true}
	state.NextAction = &NextAction{Code: "run_pipeline", Command: `no-mistakes axi run --intent "<what the user set out to accomplish>"`}
	state.Recovered, state.Changed = true, !exists || gateHead != callerHead || len(stack) > 0
	return state
}

func restartArtifact(message string) bool {
	// Only daemon lifecycle artifacts qualify, not arbitrary pipeline errors.
	return message == "daemon shutting down" || message == "daemon is shutting down" ||
		message == "daemon crashed during execution" || message == "daemon restarted" || message == "daemon restarted while run was active"
}

func releaseArchiveRef(runID, head string) string {
	return "refs/no-mistakes/release/" + runID + "/" + head
}

func rawCommitRef(ctx context.Context, dir, ref string) (string, bool, error) {
	if _, err := git.Run(ctx, dir, "symbolic-ref", "-q", ref); err == nil {
		return "", true, fmt.Errorf("symbolic ref")
	}
	head, exists, err := git.ExactRefTarget(ctx, dir, ref)
	if err != nil || !exists {
		return head, exists, err
	}
	if typ, err := git.Run(ctx, dir, "cat-file", "-t", head); err != nil || typ != "commit" {
		return "", true, fmt.Errorf("not a commit")
	}
	return head, true, nil
}
