package steps

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Revalidation of a head rewritten outside the run. While the CI monitor
// polls an open pull request, every observation of the branch head is checked
// against the set of heads this run owns: the recorded head, the durable
// last-pushed head, and the caller worktree's HEAD. A head the run owns
// nowhere means the branch was rewritten outside the run - typically a native
// stack rebase or a partial stack merge - and continuing to report checks for
// it would validate a commit the run never reviewed. The monitor then adopts
// the live head through the same durable path a CI repair uses and restarts
// the run at Review.

// maxAdoptionFetchAttempts bounds how often the adoption path re-reads and
// re-fetches the pull request branch while it keeps moving. Two attempts
// tolerate one move between observation and fetch; a branch still moving
// after that refuses rather than adopting a head nobody can name twice.
const maxAdoptionFetchAttempts = 2

// ownValidatedHeads lists every head this run can call its own: the recorded
// head first (it is the primary head merged-proof validation expects), then
// the durable last-pushed head, then the caller worktree's HEAD. A read
// failure simply omits that source - a head that cannot be proven owned is
// not owned.
func (s *CIStep) ownValidatedHeads(sctx *pipeline.StepContext) []string {
	heads := []string{strings.TrimSpace(sctx.Run.HeadSHA)}
	if run, err := sctx.DB.GetRun(sctx.Run.ID); err == nil && run != nil && run.LastPushedSHA != nil {
		if pushed := strings.TrimSpace(*run.LastPushedSHA); pushed != "" && !containsFold(heads, pushed) {
			heads = append(heads, pushed)
		}
	}
	if worktreeHead, err := stepGitHeadSHA(sctx); err == nil {
		if head := strings.TrimSpace(worktreeHead); head != "" && !containsFold(heads, head) {
			heads = append(heads, head)
		}
	}
	return heads
}

// ciRunOwnsHead reports whether head is one of the run's own heads. The
// durable record is re-read on every call: publishRunHead advances it through
// the database, so the in-memory run alone would miss a repair this run
// published.
func (s *CIStep) ciRunOwnsHead(sctx *pipeline.StepContext, head string) bool {
	for _, owned := range s.ownValidatedHeads(sctx) {
		if owned != "" && strings.EqualFold(owned, strings.TrimSpace(head)) {
			return true
		}
	}
	return false
}

// adoptableHeadRefusal names why the run worktree cannot be adopted onto the
// live head, or "" when adoption is safe. Two conditions refuse: the worktree
// is dirty (a reset would silently discard work this run never recorded), or
// the worktree holds commits that are neither on the live head nor recorded
// as published - content no observer can attribute to this run, which only an
// operator may decide about.
func adoptableHeadRefusal(sctx *pipeline.StepContext, liveHead string) string {
	status, err := stepGitRun(sctx, "status", "--porcelain")
	if err != nil {
		return fmt.Sprintf("the run worktree state could not be read: %v", err)
	}
	if strings.TrimSpace(status) != "" {
		return "the run worktree is not clean, so adopting the live head would discard uncommitted work"
	}
	localPaths, err := stepGitRunRaw(sctx, "ls-files", "--others", "-z")
	if err != nil {
		return fmt.Sprintf("local untracked paths could not be read: %v", err)
	}
	targetPaths, err := stepGitRunRaw(sctx, "ls-tree", "-r", "--name-only", "-z", liveHead)
	if err != nil {
		return fmt.Sprintf("the adopted tree could not be read: %v", err)
	}
	gitInfo, err := os.Lstat(filepath.Join(sctx.WorkDir, ".git"))
	if err != nil {
		return fmt.Sprintf("the worktree filesystem case sensitivity could not be read: %v", err)
	}
	aliasInfo, err := os.Lstat(filepath.Join(sctx.WorkDir, ".GIT"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Sprintf("the worktree filesystem case sensitivity could not be read: %v", err)
	}
	ignoreCase := err == nil && os.SameFile(gitInfo, aliasInfo)
	if local := adoptionPathCollision(localPaths, targetPaths, ignoreCase); local != "" {
		return fmt.Sprintf("local untracked or ignored path %q collides with the adopted tree", local)
	}
	worktreeHead, err := stepGitHeadSHA(sctx)
	if err != nil {
		return fmt.Sprintf("the run worktree HEAD could not be resolved: %v", err)
	}
	exclusions := []string{"rev-list", worktreeHead, "^" + strings.TrimSpace(liveHead)}
	if run, err := sctx.DB.GetRun(sctx.Run.ID); err == nil && run != nil && run.LastPushedSHA != nil {
		if pushed := strings.TrimSpace(*run.LastPushedSHA); pushed != "" && !strings.EqualFold(pushed, liveHead) {
			exclusions = append(exclusions, "^"+pushed)
		}
	}
	orphans, err := stepGitRun(sctx, exclusions...)
	if err != nil {
		return fmt.Sprintf("the worktree's relationship to the live head could not be proven: %v", err)
	}
	if strings.TrimSpace(orphans) != "" {
		return "the run worktree holds commits that are neither on the live head nor recorded as published"
	}
	return ""
}

func adoptionPathCollision(localPaths, targetPaths string, ignoreCase bool) string {
	targetFiles := make(map[string]bool)
	for target := range strings.SplitSeq(targetPaths, "\x00") {
		if ignoreCase {
			target = strings.ToLower(target)
		}
		if target == "" {
			continue
		}
		targetFiles[target] = true
		for parent := path.Dir(target); parent != "."; parent = path.Dir(parent) {
			if _, exists := targetFiles[parent]; exists {
				break
			}
			targetFiles[parent] = false
		}
	}
	for local := range strings.SplitSeq(localPaths, "\x00") {
		local = strings.TrimSuffix(local, "/")
		if local == "" {
			continue
		}
		key := local
		if ignoreCase {
			key = strings.ToLower(key)
		}
		_, collision := targetFiles[key]
		for parent := path.Dir(key); !collision && parent != "."; parent = path.Dir(parent) {
			collision = targetFiles[parent]
		}
		if collision {
			return local
		}
	}
	return ""
}

// parkPublishedHeadRewrite parks the run as ask-user: no head is adopted, the
// recorded head, the worktree head, and the live head are all named in the
// finding, and the decision is the operator's. Callers route it through
// ciTerminalRepairOutcome, so findings a human left unselected at an earlier CI
// gate reach this gate too rather than vanishing with their decision.
func parkPublishedHeadRewrite(recorded, worktreeHead, liveHead, reason string) *pipeline.StepOutcome {
	findings := Findings{
		Summary: "The pull request branch moved to a head this run owns nowhere; decide before anything is adopted",
		Items: []types.Finding{{
			ID:       types.FindingIDCIHeadRewrite,
			Severity: types.FindingSeverityWarning,
			Action:   types.ActionAskUser,
			Category: types.FindingCategoryCIHeadRewrite,
			Description: fmt.Sprintf(
				"the pull request branch head is now %s, but the run recorded %s and its worktree sits at %s: %s. Nothing was adopted and no files or refs were changed; adopt the live head only once the stray commits are accounted for.",
				shortSHA(liveHead), shortSHA(recorded), shortSHA(worktreeHead), reason,
			),
		}},
	}
	encoded, _ := types.MarshalFindingsJSON(findings)
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: encoded}
}

// adoptPublishedHeadRewrite moves the run onto the rewritten head the way a
// CI repair revalidates: the live head is fetched from the push target and
// verified again just before the restart, a fetched head the run already owns
// is not adopted at all (nil outcome: the caller keeps polling), the guard
// refuses anything unattributable, the pull request's live base is resolved
// while nothing has changed yet, recordRevalidationHead advances the run head
// durably and clears the review approval BEFORE the worktree moves (a durable
// write that fails must leave the branch and worktree on the reviewed head,
// since the worktree head is one of the heads the run is credited with owning),
// the outcome restarts the run at Review. Intent and
// Rebase are deliberately skipped (their content is base-relative and the
// restart re-decides it), and the PR step will only refresh the body of the
// existing pull request.
func (s *CIStep) adoptPublishedHeadRewrite(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, observer, liveHead string, prOpen bool) (*pipeline.StepOutcome, error) {
	recorded := strings.TrimSpace(sctx.Run.HeadSHA)
	if err := setCIMonitorReadiness(sctx, false, false); err != nil {
		return nil, err
	}
	if !prOpen {
		sctx.Log(fmt.Sprintf("warning: %s %s, but the pull request state could not be read as open: no check result is read this poll and nothing is adopted until it can be", observer, shortSHA(liveHead)))
		return nil, nil
	}
	sctx.Log(fmt.Sprintf("pull request branch head moved outside the run (%s -> %s); adopting the live head and restarting at Review", shortSHA(recorded), shortSHA(liveHead)))

	target, err := fetchVerifiedPublishedHead(sctx)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: not adopting anything this poll: %v", err))
		return nil, nil
	}
	if s.ciRunOwnsHead(sctx, target) {
		sctx.Log(fmt.Sprintf("warning: not adopting anything this poll: %s %s, but the push target serves %s, which this run owns - no check result for %s can be read, so the monitor keeps waiting for the two to agree", observer, shortSHA(liveHead), shortSHA(target), shortSHA(liveHead)))
		return nil, nil
	}
	worktreeHead, err := stepGitHeadSHA(sctx)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: not adopting %s this poll: the run worktree head could not be resolved: %v", shortSHA(target), err))
		return nil, nil
	}
	if reason := adoptableHeadRefusal(sctx, target); reason != "" {
		sctx.Log(fmt.Sprintf("not adopting %s: %s", shortSHA(target), reason))
		park := parkPublishedHeadRewrite(recorded, worktreeHead, target, reason)
		return ciTerminalRepairOutcome(park, Findings{}, sctx.DeferredFindings), nil
	}
	_, err = resolveLivePRBase(sctx, host, pr)
	if errors.Is(err, errLivePRBaseUnread) && !pluginContractBroken(err) {
		sctx.Log(fmt.Sprintf("warning: not adopting %s this poll: %v", shortSHA(target), err))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.recordRevalidationHead(sctx, target, fmt.Sprintf("adopted rewritten pull request head %s (superseding %s); revalidation from Review required", shortSHA(target), shortSHA(recorded)), false); err != nil {
		return nil, err
	}
	if _, err := stepGitRun(sctx, "reset", "--hard", target); err != nil {
		return nil, fmt.Errorf("move the run worktree to the adopted head %s: %w", shortSHA(target), err)
	}

	return &pipeline.StepOutcome{
		RestartFrom: types.StepReview,
		Findings:    sctx.DeferredFindings,
	}, nil
}

func (s *CIStep) PrepareRestart(sctx *pipeline.StepContext, restartFrom types.StepName) error {
	if restartFrom != types.StepReview {
		return nil
	}
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return fmt.Errorf("cannot align the pull request's live base before Review: %s", reason)
	}
	if _, ok := host.(scm.PRBaseBranchReader); !ok {
		return nil
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return err
	}
	if sctx.Run.PRURL == nil || strings.TrimSpace(*sctx.Run.PRURL) == "" {
		return fmt.Errorf("cannot align the pull request's live base before Review: run has no PR URL")
	}
	prURL := strings.TrimSpace(*sctx.Run.PRURL)
	number, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return err
	}
	liveBase, err := resolveLivePRBase(sctx, host, &scm.PR{Number: number, URL: prURL})
	if err != nil {
		return err
	}
	return applyRunPRBase(sctx, liveBase)
}

// fetchVerifiedPublishedHead fetches the pull request branch from the push
// target and returns its freshly verified head. The fetch writes only
// FETCH_HEAD - no branch or worktree ref moves until the caller commits to
// the adoption - and each attempt re-reads the live head first, so the
// returned head is the branch's newest observable tip, not a stale one.
func fetchVerifiedPublishedHead(sctx *pipeline.StepContext) (string, error) {
	pushURL := resolvePushURL(sctx)
	branch := strings.TrimPrefix(normalizedBranchRef(sctx.Run.Branch), "refs/heads/")
	var lastErr error
	for attempt := 0; attempt < maxAdoptionFetchAttempts; attempt++ {
		observed, err := publishedBranchHead(sctx)
		if err != nil {
			return "", fmt.Errorf("re-read the pull request branch head before adopting it: %w", err)
		}
		bounded, cancel := publishedHeadBoundedContext(sctx)
		if _, err := stepGitRun(bounded, "fetch", pushURL, branch); err != nil {
			cancel()
			return "", fmt.Errorf("fetch %s from the push target: %w", branch, err)
		}
		fetched, err := stepGitRun(bounded, "rev-parse", "FETCH_HEAD^{commit}")
		cancel()
		if err != nil {
			return "", fmt.Errorf("resolve the fetched pull request branch head: %w", err)
		}
		fetched = strings.TrimSpace(fetched)
		if fetched == observed {
			return fetched, nil
		}
		lastErr = fmt.Errorf("the pull request branch moved from %s to %s while it was being fetched", shortSHA(observed), shortSHA(fetched))
	}
	return "", lastErr
}

// errLivePRBaseUnread marks a live-base read the forge did not answer -
// transport, auth, or rate limit. It says nothing about the head, so the
// adoption treats it the way it treats every other failed read in that
// function: warn and let the next poll try again. A successful read reporting
// no base, or an unusable branch name, is permanent and still fails closed.
var errLivePRBaseUnread = errors.New("the pull request's live base branch could not be read")

// resolveLivePRBase reads the pull request's live forge base. A
// host that cannot report a base at all resolves to "" and persists nothing.
func resolveLivePRBase(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) (string, error) {
	reader, ok := host.(scm.PRBaseBranchReader)
	if !ok {
		return "", nil
	}
	liveBase, err := reader.GetPRBaseBranch(sctx.Ctx, pr)
	if err != nil {
		return "", fmt.Errorf("read the pull request's live base branch before restarting at Review: %w: %w", errLivePRBaseUnread, err)
	}
	liveBase = strings.TrimSpace(liveBase)
	if liveBase == "" {
		return "", fmt.Errorf("the pull request reported no live base branch before restarting at Review")
	}
	if _, err := ValidateRunPRBaseBranchName(liveBase); err != nil {
		return "", fmt.Errorf("the pull request's live base branch is not a usable branch name: %w", err)
	}
	return liveBase, nil
}

// applyRunPRBase sets the run's persisted per-run base to the base
// resolveLivePRBase read, whenever that differs from the base the restart will
// actually SCOPE by (effectivePRBaseBranch). That is the comparison the alignment
// exists for: without it a restarted Review scopes the change against a branch
// the pull request no longer targets, and a retargeted layer re-reviews every
// layer beneath it. Comparing against the effective base instead missed exactly
// that case, because a pushed pr.base_branch can equal the live base while the
// scoping base is still the repository default. The write deliberately
// supersedes an operator's earlier --base-branch: this is a restart-time
// alignment to the pull request as it now is.
func applyRunPRBase(sctx *pipeline.StepContext, liveBase string) error {
	if liveBase == "" {
		return nil
	}
	liveBase, err := ValidateRunPRBaseBranchName(liveBase)
	if err != nil {
		return fmt.Errorf("the pull request's live base branch is not a usable branch name: %w", err)
	}
	current := effectivePRBaseBranch(sctx)
	if current == liveBase {
		return nil
	}
	if err := sctx.DB.UpdateRunPRBaseBranch(sctx.Run.ID, liveBase); err != nil {
		return err
	}
	sctx.Run.PRBaseBranch = &liveBase
	sctx.Log(fmt.Sprintf("run base branch updated to the pull request's live base %q (was %q)", liveBase, current))
	return nil
}

func containsFold(heads []string, head string) bool {
	for _, candidate := range heads {
		if strings.EqualFold(candidate, head) {
			return true
		}
	}
	return false
}
