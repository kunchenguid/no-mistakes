package steps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// autoFixCI runs the agent to fix CI failures and/or merge conflicts, then
// commits and pushes to the configured push remote.
// Returns (true, nil) when changes were committed and pushed, (false, nil)
// when the agent produced no changes, or (false, err) on failure.
func (s *CIStep) autoFixCI(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, failingNames []string, mergeConflict bool) (pushed bool, retErr error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	rebaseBaseSHA, rebaseBaseResolved := resolveDefaultBranchTip(ctx, sctx.WorkDir, sctx.Repo.UpstreamURL, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	promptBaseSHA := baseSHA
	if mergeConflict {
		promptBaseSHA = rebaseBaseSHA
		if !rebaseBaseResolved {
			return false, fmt.Errorf("resolve current base branch tip before CI conflict repair")
		}
	}

	const maxLogBytes = 32 * 1024
	var logOutput string
	if host.Capabilities().FailedCheckLogs {
		raw, err := host.FetchFailedCheckLogs(ctx, pr, sctx.Run.Branch, sctx.Run.HeadSHA, failingNames)
		if err != nil && err != scm.ErrUnsupported {
			slog.Warn("failed to fetch CI logs", "err", err)
		}
		if raw != "" {
			logOutput = trimLogOutput(strings.TrimSpace(raw), maxLogBytes)
		}
	}

	// Build prompt based on what issues are present
	var promptIntro string
	var promptRules string
	switch {
	case len(failingNames) > 0 && mergeConflict:
		promptIntro = "The following CI checks have failed and the PR has merge conflicts with the base branch. Diagnose and fix the CI issues, then rebase onto the base branch and resolve the merge conflicts."
		promptRules = `- You MUST produce file changes that fix the failing checks. Do not conclude that nothing needs to change.
		- If a test fails only on a specific OS (e.g. Windows CRLF, path separators), fix the test to be cross-platform.
		- If a test is flaky, make it deterministic.
		- Make the smallest correct root-cause fix.
		- Do not refactor beyond what is needed for that root-cause fix.
		- Verify the fix by running the most relevant commands locally before finishing.`
	case mergeConflict:
		promptIntro = "The PR has merge conflicts with the base branch. Rebase onto the base branch and resolve the merge conflicts."
		promptRules = `- Resolve the merge conflicts by applying the minimal necessary changes.
		- Do not make unrelated file edits.
		- Verify the rebase completes cleanly before finishing.`
	default:
		promptIntro = "The following CI checks have failed on this PR. Diagnose and fix the issues."
		promptRules = `- You MUST produce file changes that fix the failing checks. Do not conclude that nothing needs to change.
		- If a test fails only on a specific OS (e.g. Windows CRLF, path separators), fix the test to be cross-platform.
		- If a test is flaky, make it deterministic.
		- Make the smallest correct root-cause fix.
		- Do not refactor beyond what is needed for that root-cause fix.
		- Verify the fix by running the most relevant commands locally before finishing.`
	}

	prompt := fmt.Sprintf(
		`%s

Context:
- branch: %s
- base commit: %s
- target commit: %s
- PR number: %s
- failing checks: %s
- merge conflict: %v

		Rules:
		%s`,
		promptIntro,
		sctx.Run.Branch,
		promptBaseSHA,
		sctx.Run.HeadSHA,
		pr.Number,
		strings.Join(failingNames, ", "),
		mergeConflict,
		promptRules,
	)
	if mergeConflict {
		prompt += fmt.Sprintf("\n- rebase target commit: %s", rebaseBaseSHA)
	}
	if logOutput != "" {
		prompt += fmt.Sprintf(`

CI logs:
%s`, logOutput)
	}
	prompt += userIntentPromptSection(sctx)

	if err := validateCIRepairStartingState(sctx); err != nil {
		return false, err
	}
	s.verifiedCandidateHead = ""
	s.verifiedCandidateTree = ""
	tier := s.ciRepairTier(sctx)
	sctx.Log(fmt.Sprintf("running agent to fix CI issues (tier %d)...", tier))
	if len(s.activeCIRepairPlan.Issues) > 0 {
		repairIDs, beginErr := s.beginCIRepairs(sctx, s.activeCIRepairPlan, s.activeCIRepairBudget)
		if beginErr != nil {
			return false, beginErr
		}
		s.activeCIRepairIDs = repairIDs
	}
	priorAttempts, priorErr := ciInvocationAttemptIDs(sctx, s.activeCIRepairIDs)
	if priorErr != nil {
		return false, priorErr
	}
	candidateBeforeRepair, err := captureCICandidate(sctx)
	if err != nil {
		return false, fmt.Errorf("snapshot candidate before CI repair: %w", err)
	}
	snapshotRetained := false
	publicationAccepted := false
	defer func() {
		rollbackContext := *sctx
		rollbackContext.Ctx = context.WithoutCancel(sctx.Ctx)
		if retErr != nil && !publicationAccepted {
			s.verifiedCandidateHead = ""
			s.verifiedCandidateTree = ""
			if restoreErr := candidateBeforeRepair.restore(&rollbackContext); restoreErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("restore candidate after failed CI repair: %w", restoreErr))
			}
		}
		if !snapshotRetained {
			candidateBeforeRepair.cleanup(&rollbackContext)
		}
	}()
	_, err = sctx.InvokeAgentTier(types.PurposeUnstructuredCIRepair, tier, agent.RunOpts{
		Prompt:  prompt,
		CWD:     sctx.WorkDir,
		OnChunk: sctx.LogChunk,
	})
	linked, linkErr := linkCIInvocationAfter(sctx, s.activeCIRepairIDs, types.PurposeUnstructuredCIRepair, true, priorAttempts)
	if linkErr != nil {
		if err != nil {
			return false, fmt.Errorf("%w; additionally agent CI fix failed: %v", linkErr, err)
		}
		return false, linkErr
	}
	if err != nil {
		return false, fmt.Errorf("agent CI fix: %w", err)
	}
	if !linked {
		return false, &ciJournalError{operation: "link hosted CI invocation", err: fmt.Errorf("no journaled %s attempt in current round", types.PurposeUnstructuredCIRepair)}
	}
	if err := candidateBeforeRepair.restoreSharedRefsForCandidate(sctx); err != nil {
		return false, fmt.Errorf("restore shared refs after CI repair: %w", err)
	}

	candidateChanged, candidateHead, candidateTree, err := s.prepareCICandidate(sctx, mergeConflict, rebaseBaseSHA)
	if err != nil {
		return false, err
	}
	if !candidateChanged {
		integrityContext := *sctx
		integrityContext.Ctx = context.WithoutCancel(sctx.Ctx)
		if err := candidateBeforeRepair.validate(&integrityContext); err != nil {
			return false, fmt.Errorf("CI repair reported no candidate changes but mutated state: %w", err)
		}
		return false, nil
	}
	verificationCandidate, err := captureCICandidate(sctx)
	if err != nil {
		return false, fmt.Errorf("snapshot CI candidate before verification: %w", err)
	}
	verificationErr := s.verifyCIPatch(sctx, baseSHA)
	integrityContext := *sctx
	integrityContext.Ctx = context.WithoutCancel(sctx.Ctx)
	integrityErr := verificationCandidate.validate(&integrityContext)
	verificationCandidate.cleanup(sctx)
	if integrityErr != nil {
		integrityErr = fmt.Errorf("CI patch changed during verification: %w", integrityErr)
		if verificationErr != nil {
			return false, errors.Join(integrityErr, fmt.Errorf("CI patch failed verification: %w", verificationErr))
		}
		return false, integrityErr
	}
	if verificationErr != nil {
		return false, fmt.Errorf("CI patch failed verification: %w", verificationErr)
	}
	if err := candidateBeforeRepair.validateIgnoredPathsForCandidate(sctx); err != nil {
		return false, fmt.Errorf("validate pre-existing ignored CI state: %w", err)
	}
	s.verifiedCandidateHead = candidateHead
	s.verifiedCandidateTree = candidateTree
	pushed, err = s.commitAndPushWithSnapshot(sctx, &candidateBeforeRepair)
	publication, publicationErr := sctx.DB.LatestPublication(sctx.Run.ID, db.PublicationKindCI)
	if publicationErr != nil {
		snapshotRetained = true
		if err != nil {
			return false, errors.Join(err, &ciJournalError{operation: "confirm CI publication state", err: publicationErr})
		}
		return false, &ciJournalError{operation: "confirm CI publication state", err: publicationErr}
	}
	if publication != nil {
		publicationAccepted = publication.State == db.PublicationStateAccepted || publication.State == db.PublicationStateCompleted
		if publication.CleanupSnapshotDir == candidateBeforeRepair.rootDir {
			snapshotRetained = true
		}
	}
	if err != nil {
		return false, err
	}
	if publication == nil || publication.State != db.PublicationStateCompleted || publication.SealSHA != sctx.Run.HeadSHA {
		return false, &ciJournalError{operation: "confirm completed CI publication", err: fmt.Errorf("publication cleanup did not complete")}
	}
	return pushed, nil
}
func validateCIRepairStartingState(sctx *pipeline.StepContext) error {
	status, err := stepGitRun(sctx, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect pre-existing CI candidate state: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return nil
	}
	return fmt.Errorf("refusing CI repair with pre-existing staged, unstaged, or untracked changes; preserve or remove them before fixing CI")
}

// prepareCICandidate freezes the agent's candidate in the index before local
// checks and independent verification. A clean changed HEAD (for example a
// completed rebase) is a candidate just as much as a dirty worktree.
func (s *CIStep) prepareCICandidate(sctx *pipeline.StepContext, mergeConflict bool, rebaseBaseSHA string) (bool, string, string, error) {
	status, err := stepGitRun(sctx, "status", "--porcelain")
	if err != nil {
		return false, "", "", fmt.Errorf("check CI changes: %w", err)
	}
	headSHA, err := stepGitHeadSHA(sctx)
	if err != nil {
		return false, "", "", fmt.Errorf("resolve CI candidate HEAD: %w", err)
	}
	if strings.TrimSpace(status) == "" && headSHA == sctx.Run.HeadSHA {
		return false, "", "", nil
	}
	if mergeConflict {
		if err := validateCIConflictResolution(sctx, rebaseBaseSHA); err != nil {
			return false, "", "", err
		}
	}
	if strings.TrimSpace(status) != "" {
		if _, err := stepGitRun(sctx, "add", "-A"); err != nil {
			return false, "", "", fmt.Errorf("stage CI candidate: %w", err)
		}
	}
	tree, err := stepGitRun(sctx, "write-tree")
	if err != nil {
		return false, "", "", fmt.Errorf("snapshot CI candidate: %w", err)
	}
	return true, headSHA, tree, nil
}

func validateCIConflictResolution(sctx *pipeline.StepContext, rebaseBaseSHA string) error {
	unmerged, err := stepGitRun(sctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return fmt.Errorf("validate conflict state: %w", err)
	}
	if strings.TrimSpace(unmerged) != "" {
		return fmt.Errorf("CI conflict repair left unresolved paths: %s", strings.Join(strings.Fields(unmerged), ", "))
	}
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		path, err := stepGitRun(sctx, "rev-parse", "--git-path", state)
		if err != nil {
			return fmt.Errorf("resolve %s state path: %w", state, err)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(sctx.WorkDir, path)
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("CI conflict repair left a rebase in progress")
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect rebase state: %w", err)
		}
	}
	if strings.TrimSpace(rebaseBaseSHA) == "" {
		return fmt.Errorf("CI conflict repair has no resolved base tip to validate")
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", rebaseBaseSHA, "HEAD"); err != nil {
		return fmt.Errorf("CI conflict repair did not incorporate base tip %s: %w", shortSHA(rebaseBaseSHA), err)
	}
	return nil
}

func validatePreparedCICandidate(sctx *pipeline.StepContext, wantHead, wantTree string) error {
	headSHA, err := stepGitHeadSHA(sctx)
	if err != nil {
		return err
	}
	if headSHA != wantHead {
		return fmt.Errorf("candidate HEAD changed from %s to %s", shortSHA(wantHead), shortSHA(headSHA))
	}
	status, err := stepGitRun(sctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(status, "\n") {
		if line == "" {
			continue
		}
		if len(line) < 2 || line[0:2] == "??" || line[1] != ' ' {
			return fmt.Errorf("worktree changed after candidate snapshot: %s", line)
		}
	}
	gotTree, err := stepGitRun(sctx, "write-tree")
	if err != nil {
		return err
	}
	if gotTree != wantTree {
		return fmt.Errorf("candidate tree changed from %s to %s", shortSHA(wantTree), shortSHA(gotTree))
	}
	return nil
}

type ciCandidateSnapshot struct {
	head             string
	headRef          string
	rebaseHeadRef    string
	indexTree        string
	status           string
	trackedDiff      string
	rootDir          string
	worktreeDir      string
	rebaseStateDir   string
	refs             map[string]rebaseRefState
	ignoredPaths     map[string]struct{}
	rebaseInProgress bool
}

func captureCICandidate(sctx *pipeline.StepContext) (ciCandidateSnapshot, error) {
	var snapshot ciCandidateSnapshot
	var err error
	snapshot.head, err = stepGitHeadSHA(sctx)
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	snapshot.headRef, err = stepGitRun(sctx, "rev-parse", "--symbolic-full-name", "HEAD")
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("resolve HEAD reference: %w", err)
	}
	snapshot.indexTree, err = stepGitRun(sctx, "write-tree")
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("snapshot index: %w", err)
	}
	snapshot.status, err = stepGitRun(sctx, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("snapshot status: %w", err)
	}
	snapshot.trackedDiff, err = stepGitRun(sctx, "diff", "--binary", "--no-ext-diff", "--")
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("snapshot tracked worktree: %w", err)
	}
	snapshot.rebaseInProgress, err = ciRebaseState(sctx)
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("snapshot rebase state: %w", err)
	}
	snapshot.refs, err = captureCIRefs(sctx)
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("snapshot shared refs: %w", err)
	}
	snapshot.ignoredPaths, err = captureCIIgnoredPaths(sctx)
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("snapshot ignored paths: %w", err)
	}
	gitDir, err := stepGitRun(sctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("resolve Git directory for candidate snapshot: %w", err)
	}
	snapshot.rootDir, err = os.MkdirTemp(gitDir, "no-mistakes-ci-candidate-")
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("create candidate snapshot: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			snapshot.cleanup(sctx)
		}
	}()
	snapshot.worktreeDir = filepath.Join(snapshot.rootDir, "worktree")
	if err := os.Mkdir(snapshot.worktreeDir, 0o700); err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("create worktree snapshot: %w", err)
	}
	if err := copyCIWorktree(sctx.WorkDir, snapshot.worktreeDir); err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("snapshot candidate worktree: %w", err)
	}
	snapshot.rebaseStateDir = filepath.Join(snapshot.rootDir, "git-state")
	if err := os.Mkdir(snapshot.rebaseStateDir, 0o700); err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("create rebase snapshot: %w", err)
	}
	if err := snapshotCIRebaseState(sctx, snapshot.rebaseStateDir); err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("snapshot rebase metadata: %w", err)
	}
	if err := snapshotCIGitMetadata(sctx, snapshot.rebaseStateDir); err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("snapshot shared Git metadata: %w", err)
	}
	snapshot.rebaseHeadRef = snapshotCIRebaseHeadRef(snapshot.rebaseStateDir)
	complete = true
	return snapshot, nil
}
func captureCIRefs(sctx *pipeline.StepContext) (map[string]rebaseRefState, error) {
	output, err := stepGitRun(sctx, "for-each-ref", "--format=%(refname)%09%(objectname)%09%(symref)")
	if err != nil {
		return nil, err
	}
	refs := make(map[string]rebaseRefState)
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed ref snapshot line %q", line)
		}
		state := rebaseRefState{oid: fields[1]}
		if len(fields) == 3 {
			state.symref = fields[2]
		}
		refs[fields[0]] = state
	}
	return refs, nil
}
func captureCIIgnoredPaths(sctx *pipeline.StepContext) (map[string]struct{}, error) {
	output, err := stepGitRun(sctx, "status", "--porcelain=v1", "-z", "--ignored=matching", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	paths := make(map[string]struct{})
	for _, entry := range strings.Split(strings.TrimSuffix(output, "\x00"), "\x00") {
		if !strings.HasPrefix(entry, "!! ") {
			continue
		}
		path := filepath.Clean(strings.TrimSuffix(entry[3:], "/"))
		if path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("unsafe ignored path %q", entry[3:])
		}
		paths[path] = struct{}{}
	}
	return paths, nil
}

func snapshotCIRebaseHeadRef(rebaseStateDir string) string {
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		data, err := os.ReadFile(filepath.Join(rebaseStateDir, state, "head-name"))
		if err != nil {
			continue
		}
		ref := strings.TrimSpace(string(data))
		if strings.HasPrefix(ref, "refs/heads/") {
			return ref
		}
	}
	return ""
}

func (snapshot ciCandidateSnapshot) restoreSharedRefsForCandidate(sctx *pipeline.StepContext) error {
	if err := restoreCIGitMetadata(sctx, snapshot.rebaseStateDir); err != nil {
		return fmt.Errorf("restore shared Git metadata: %w", err)
	}
	preserve := make(map[string]string)
	if snapshot.headRef != "HEAD" {
		head, err := stepGitHeadSHA(sctx)
		if err != nil {
			return err
		}
		preserve[snapshot.headRef] = head
	} else if snapshot.rebaseHeadRef != "" {
		rebaseInProgress, err := ciRebaseState(sctx)
		if err != nil {
			return err
		}
		if !rebaseInProgress {
			currentRef, err := stepGitRun(sctx, "rev-parse", "--symbolic-full-name", "HEAD")
			if err != nil {
				return err
			}
			if currentRef == snapshot.rebaseHeadRef {
				head, err := stepGitHeadSHA(sctx)
				if err != nil {
					return err
				}
				preserve[currentRef] = head
			}
		}
	}
	return snapshot.restoreSharedRefs(sctx, preserve)
}

type ciRefRestoreHookContextKey struct{}

func (snapshot ciCandidateSnapshot) restoreSharedRefs(sctx *pipeline.StepContext, preserve map[string]string) error {
	current, err := captureCIRefs(sctx)
	if err != nil {
		return err
	}
	var restoreErr error
	for ref, observed := range current {
		if _, keep := preserve[ref]; keep {
			continue
		}
		if _, existed := snapshot.refs[ref]; existed {
			continue
		}
		preservePending, err := shouldPreserveCIRepublishRef(sctx, ref, observed.oid)
		if err != nil {
			restoreErr = errors.Join(restoreErr, err)
			continue
		}
		if preservePending {
			continue
		}
		runCIRefRestoreHook(sctx.Ctx, ref)
		if err := restoreRebaseRefCAS(sctx.Ctx, sctx.WorkDir, ref, rebaseRefState{}, false, observed, true); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("delete repair-created ref %s with lease: %w", ref, err))
		}
	}
	for ref, want := range snapshot.refs {
		if _, keep := preserve[ref]; keep {
			continue
		}
		observed, exists := current[ref]
		if exists && observed == want {
			continue
		}
		runCIRefRestoreHook(sctx.Ctx, ref)
		if err := restoreRebaseRefCAS(sctx.Ctx, sctx.WorkDir, ref, want, true, observed, exists); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore shared ref %s with lease: %w", ref, err))
		}
	}
	return restoreErr
}

func runCIRefRestoreHook(ctx context.Context, ref string) {
	if hook, ok := ctx.Value(ciRefRestoreHookContextKey{}).(func(string)); ok && hook != nil {
		hook(ref)
	}
}

func shouldPreserveCIRepublishRef(sctx *pipeline.StepContext, ref, sha string) (bool, error) {
	if ref != ciRepublishPendingRef(sctx) {
		return false, nil
	}
	publication, err := sctx.DB.LatestPublication(sctx.Run.ID, db.PublicationKindCI)
	if err != nil {
		return false, err
	}
	return publication != nil && publication.SealSHA == sha, nil
}

func snapshotCIRebaseState(sctx *pipeline.StepContext, destination string) error {
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		path, err := ciStatePath(sctx, state)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyCICandidatePath(path, filepath.Join(destination, state)); err != nil {
			return err
		}
	}
	return nil
}

func ciStatePath(sctx *pipeline.StepContext, state string) (string, error) {
	path, err := stepGitRun(sctx, "rev-parse", "--git-path", state)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(sctx.WorkDir, path)
	}
	return path, nil
}

func (snapshot ciCandidateSnapshot) restoreCIRebaseState(sctx *pipeline.StepContext) error {
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		path, err := ciStatePath(sctx, state)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		saved := filepath.Join(snapshot.rebaseStateDir, state)
		if _, err := os.Lstat(saved); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyCICandidatePath(saved, path); err != nil {
			return err
		}
	}
	return nil
}

type ciGitMetadataPath struct {
	live  string
	saved string
}

func ciGitMetadataPaths(sctx *pipeline.StepContext, snapshotDir string) ([]ciGitMetadataPath, error) {
	commonDir, err := stepGitRun(sctx, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	gitDir, err := stepGitRun(sctx, "rev-parse", "--git-dir")
	if err != nil {
		return nil, err
	}
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Clean(filepath.Join(sctx.WorkDir, path))
	}
	commonDir = resolve(commonDir)
	gitDir = resolve(gitDir)
	return []ciGitMetadataPath{
		{live: filepath.Join(commonDir, "config"), saved: filepath.Join(snapshotDir, "git-common", "config")},
		{live: filepath.Join(commonDir, "hooks"), saved: filepath.Join(snapshotDir, "git-common", "hooks")},
		{live: filepath.Join(commonDir, "info"), saved: filepath.Join(snapshotDir, "git-common", "info")},
		{live: filepath.Join(gitDir, "config.worktree"), saved: filepath.Join(snapshotDir, "git-dir", "config.worktree")},
	}, nil
}

func snapshotCIGitMetadata(sctx *pipeline.StepContext, snapshotDir string) error {
	paths, err := ciGitMetadataPaths(sctx, snapshotDir)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if _, err := os.Lstat(path.live); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyCICandidatePath(path.live, path.saved); err != nil {
			return err
		}
	}
	return nil
}

func restoreCIGitMetadata(sctx *pipeline.StepContext, snapshotDir string) error {
	paths, err := ciGitMetadataPaths(sctx, snapshotDir)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := os.RemoveAll(path.live); err != nil {
			return err
		}
		if _, err := os.Lstat(path.saved); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyCICandidatePath(path.saved, path.live); err != nil {
			return err
		}
	}
	return nil
}

func validateCIGitMetadata(sctx *pipeline.StepContext, snapshotDir string) error {
	paths, err := ciGitMetadataPaths(sctx, snapshotDir)
	if err != nil {
		return err
	}
	var buffers ciFileCompareBuffers
	for _, path := range paths {
		savedInfo, savedErr := os.Lstat(path.saved)
		liveInfo, liveErr := os.Lstat(path.live)
		if os.IsNotExist(savedErr) && os.IsNotExist(liveErr) {
			continue
		}
		if savedErr != nil {
			return savedErr
		}
		if liveErr != nil {
			return liveErr
		}
		if savedInfo.Mode() != liveInfo.Mode() {
			return fmt.Errorf("metadata mode differs for %s", filepath.Base(path.live))
		}
		if savedInfo.IsDir() {
			if err := compareCIWorktrees(path.saved, path.live); err != nil {
				return err
			}
			continue
		}
		if err := compareCIFileContents(path.saved, path.live, &buffers); err != nil {
			return err
		}
	}
	return nil
}

func copyCIWorktree(sourceDir, destinationDir string) error {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := copyCICandidatePath(filepath.Join(sourceDir, entry.Name()), filepath.Join(destinationDir, entry.Name())); err != nil {
			return fmt.Errorf("copy candidate path %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func copyCICandidatePath(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(target, destination)
	}
	if info.IsDir() {
		if err := os.MkdirAll(destination, ciCandidateMode(info.Mode())); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyCICandidatePath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(destination, ciCandidateMode(info.Mode()))
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported file mode %s", info.Mode())
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, ciCandidateMode(info.Mode()))
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Chmod(destination, ciCandidateMode(info.Mode()))
}

func ciCandidateMode(mode os.FileMode) os.FileMode {
	return mode.Perm() | mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
}

func (snapshot ciCandidateSnapshot) restore(sctx *pipeline.StepContext) error {
	if err := snapshot.reconcileRebase(sctx); err != nil {
		return err
	}
	if err := restoreCIGitMetadata(sctx, snapshot.rebaseStateDir); err != nil {
		return fmt.Errorf("restore shared Git metadata: %w", err)
	}
	if err := snapshot.restoreSharedRefs(sctx, nil); err != nil {
		return fmt.Errorf("restore shared refs: %w", err)
	}
	if _, err := stepGitRun(sctx, "reset", "--hard"); err != nil {
		return err
	}
	if snapshot.headRef == "HEAD" {
		if _, err := stepGitRun(sctx, "checkout", "--detach", "--force", snapshot.head); err != nil {
			return err
		}
	} else {
		if _, err := stepGitRun(sctx, "symbolic-ref", "HEAD", snapshot.headRef); err != nil {
			return err
		}
	}
	if _, err := stepGitRun(sctx, "reset", "--hard", snapshot.head); err != nil {
		return err
	}
	if err := clearCIWorktree(sctx.WorkDir); err != nil {
		return fmt.Errorf("clear failed CI candidate: %w", err)
	}
	if err := copyCIWorktree(snapshot.worktreeDir, sctx.WorkDir); err != nil {
		return fmt.Errorf("restore candidate worktree: %w", err)
	}
	if _, err := stepGitRun(sctx, "read-tree", snapshot.indexTree); err != nil {
		return fmt.Errorf("restore candidate index: %w", err)
	}
	if err := snapshot.restoreCIRebaseState(sctx); err != nil {
		return fmt.Errorf("restore rebase metadata: %w", err)
	}
	return snapshot.validate(sctx)
}

func (snapshot ciCandidateSnapshot) validateIgnoredPathsForCandidate(sctx *pipeline.StepContext) error {
	for path := range snapshot.ignoredPaths {
		if _, err := stepGitRun(sctx, "ls-files", "--error-unmatch", "--", path); err == nil {
			return fmt.Errorf("repair made pre-existing ignored path %q tracked", path)
		}
		if _, err := stepGitRun(sctx, "check-ignore", "--quiet", "--no-index", "--", path); err != nil {
			return fmt.Errorf("repair changed ignore semantics for pre-existing path %q", path)
		}
	}
	return nil
}

func (snapshot ciCandidateSnapshot) restoreFilesystemAtSealedSHA(sctx *pipeline.StepContext, sha string) error {
	if err := reconcilePublicationWorktree(sctx, sha); err != nil {
		return err
	}
	currentIgnored, err := captureCIIgnoredPaths(sctx)
	if err != nil {
		return err
	}
	for path := range currentIgnored {
		if _, existed := snapshot.ignoredPaths[path]; existed {
			continue
		}
		destination := filepath.Join(sctx.WorkDir, path)
		if _, err := os.Lstat(destination); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := removeCICandidatePath(destination); err != nil {
			return fmt.Errorf("remove repair-created ignored path %q: %w", path, err)
		}
	}
	for path := range snapshot.ignoredPaths {
		source := filepath.Join(snapshot.worktreeDir, path)
		destination := filepath.Join(sctx.WorkDir, path)
		if _, err := os.Lstat(destination); err == nil {
			if err := removeCICandidatePath(destination); err != nil {
				return fmt.Errorf("replace ignored path %q: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if _, err := os.Lstat(source); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyCICandidatePath(source, destination); err != nil {
			return fmt.Errorf("restore ignored path %q: %w", path, err)
		}
	}
	return reconcilePublicationWorktree(sctx, sha)
}

func (snapshot ciCandidateSnapshot) reconcileRebase(sctx *pipeline.StepContext) error {
	current, err := ciRebaseState(sctx)
	if err != nil {
		return fmt.Errorf("inspect failed repair rebase state: %w", err)
	}
	if snapshot.rebaseInProgress || !current {
		return nil
	}
	if _, err := stepGitRun(sctx, "rebase", "--abort"); err == nil {
		return nil
	} else {
		abortErr := err
		if _, quitErr := stepGitRun(sctx, "rebase", "--quit"); quitErr != nil {
			return errors.Join(fmt.Errorf("abort failed repair rebase: %w", abortErr), fmt.Errorf("quit failed repair rebase: %w", quitErr))
		}
	}
	return nil
}

func ciRebaseState(sctx *pipeline.StepContext) (bool, error) {
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		path, err := stepGitRun(sctx, "rev-parse", "--git-path", state)
		if err != nil {
			return false, err
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(sctx.WorkDir, path)
		}
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

func clearCIWorktree(workDir string) error {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := removeCICandidatePath(filepath.Join(workDir, entry.Name())); err != nil {
			return fmt.Errorf("remove candidate path %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func removeCICandidatePath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return os.Remove(path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removeCICandidatePath(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func (snapshot ciCandidateSnapshot) validate(sctx *pipeline.StepContext) error {
	head, err := stepGitHeadSHA(sctx)
	if err != nil {
		return err
	}
	if head != snapshot.head {
		return fmt.Errorf("candidate HEAD changed from %s to %s", shortSHA(snapshot.head), shortSHA(head))
	}
	indexTree, err := stepGitRun(sctx, "write-tree")
	if err != nil {
		return err
	}
	if indexTree != snapshot.indexTree {
		return fmt.Errorf("candidate index tree %s, want %s", shortSHA(indexTree), shortSHA(snapshot.indexTree))
	}
	status, err := stepGitRun(sctx, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != snapshot.status {
		return fmt.Errorf("candidate worktree status differs from snapshot")
	}
	trackedDiff, err := stepGitRun(sctx, "diff", "--binary", "--no-ext-diff", "--")
	if err != nil {
		return err
	}
	if trackedDiff != snapshot.trackedDiff {
		return fmt.Errorf("candidate tracked worktree content differs from snapshot")
	}
	if err := compareCIWorktrees(snapshot.worktreeDir, sctx.WorkDir); err != nil {
		return fmt.Errorf("candidate filesystem differs from snapshot: %w", err)
	}
	currentRefs, err := captureCIRefs(sctx)
	if err != nil {
		return err
	}
	for ref, want := range snapshot.refs {
		if got := currentRefs[ref]; got != want {
			return fmt.Errorf("shared ref %s topology changed from %s to %s", ref, shortSHA(want.oid), shortPublicationSHA(got.oid))
		}
	}
	for ref, state := range currentRefs {
		if _, existed := snapshot.refs[ref]; existed {
			continue
		}
		preservePending, err := shouldPreserveCIRepublishRef(sctx, ref, state.oid)
		if err != nil {
			return err
		}
		if !preservePending {
			return fmt.Errorf("repair-created shared ref %s remains", ref)
		}
	}
	if err := validateCIGitMetadata(sctx, snapshot.rebaseStateDir); err != nil {
		return fmt.Errorf("shared Git metadata differs from snapshot: %w", err)
	}
	rebaseInProgress, err := ciRebaseState(sctx)
	if err != nil {
		return err
	}
	if rebaseInProgress != snapshot.rebaseInProgress {
		return fmt.Errorf("candidate rebase topology differs from snapshot")
	}
	return nil
}

type ciFileCompareBuffers struct {
	expected [32 * 1024]byte
	actual   [32 * 1024]byte
}

func compareCIWorktrees(expectedDir, actualDir string) error {
	expected, err := os.ReadDir(expectedDir)
	if err != nil {
		return err
	}
	actual, err := os.ReadDir(actualDir)
	if err != nil {
		return err
	}
	filteredActual := actual[:0]
	for _, entry := range actual {
		if entry.Name() != ".git" {
			filteredActual = append(filteredActual, entry)
		}
	}
	if len(expected) != len(filteredActual) {
		return fmt.Errorf("top-level entry count is %d, want %d", len(filteredActual), len(expected))
	}
	buffers := &ciFileCompareBuffers{}
	for i := range expected {
		if expected[i].Name() != filteredActual[i].Name() {
			return fmt.Errorf("top-level path %q, want %q", filteredActual[i].Name(), expected[i].Name())
		}
		if err := compareCICandidatePath(filepath.Join(expectedDir, expected[i].Name()), filepath.Join(actualDir, filteredActual[i].Name()), buffers); err != nil {
			return fmt.Errorf("%s: %w", expected[i].Name(), err)
		}
	}
	return nil
}

func compareCICandidatePath(expected, actual string, buffers *ciFileCompareBuffers) error {
	expectedInfo, err := os.Lstat(expected)
	if err != nil {
		return err
	}
	actualInfo, err := os.Lstat(actual)
	if err != nil {
		return err
	}
	if expectedInfo.Mode().Type() != actualInfo.Mode().Type() {
		return fmt.Errorf("type is %s, want %s", actualInfo.Mode().Type(), expectedInfo.Mode().Type())
	}
	if ciCandidateMode(expectedInfo.Mode()) != ciCandidateMode(actualInfo.Mode()) {
		return fmt.Errorf("mode is %s, want %s", ciCandidateMode(actualInfo.Mode()), ciCandidateMode(expectedInfo.Mode()))
	}
	if expectedInfo.Mode()&os.ModeSymlink != 0 {
		expectedTarget, err := os.Readlink(expected)
		if err != nil {
			return err
		}
		actualTarget, err := os.Readlink(actual)
		if err != nil {
			return err
		}
		if expectedTarget != actualTarget {
			return fmt.Errorf("symlink target is %q, want %q", actualTarget, expectedTarget)
		}
		return nil
	}
	if expectedInfo.IsDir() {
		expectedEntries, err := os.ReadDir(expected)
		if err != nil {
			return err
		}
		actualEntries, err := os.ReadDir(actual)
		if err != nil {
			return err
		}
		if len(expectedEntries) != len(actualEntries) {
			return fmt.Errorf("directory entry count is %d, want %d", len(actualEntries), len(expectedEntries))
		}
		for i := range expectedEntries {
			if expectedEntries[i].Name() != actualEntries[i].Name() {
				return fmt.Errorf("directory path %q, want %q", actualEntries[i].Name(), expectedEntries[i].Name())
			}
			if err := compareCICandidatePath(filepath.Join(expected, expectedEntries[i].Name()), filepath.Join(actual, actualEntries[i].Name()), buffers); err != nil {
				return fmt.Errorf("%s: %w", expectedEntries[i].Name(), err)
			}
		}
		return nil
	}
	if expectedInfo.Size() != actualInfo.Size() {
		return fmt.Errorf("size is %d, want %d", actualInfo.Size(), expectedInfo.Size())
	}
	return compareCIFileContents(expected, actual, buffers)
}

func compareCIFileContents(expected, actual string, buffers *ciFileCompareBuffers) error {
	expectedFile, err := os.Open(expected)
	if err != nil {
		return err
	}
	defer expectedFile.Close()
	actualFile, err := os.Open(actual)
	if err != nil {
		return err
	}
	defer actualFile.Close()
	for {
		expectedN, expectedErr := io.ReadFull(expectedFile, buffers.expected[:])
		actualN, actualErr := io.ReadFull(actualFile, buffers.actual[:])
		if expectedN != actualN || !bytes.Equal(buffers.expected[:expectedN], buffers.actual[:actualN]) {
			return fmt.Errorf("file content differs")
		}
		if expectedErr == io.EOF && actualErr == io.EOF {
			return nil
		}
		if expectedErr == io.ErrUnexpectedEOF && actualErr == io.ErrUnexpectedEOF {
			return nil
		}
		if expectedErr != nil {
			return expectedErr
		}
		if actualErr != nil {
			return actualErr
		}
	}
}

const ciPublicationSnapshotManifest = "publication-cleanup.json"

type ciRefSnapshotManifest struct {
	OID    string `json:"oid"`
	Symref string `json:"symref,omitempty"`
}

type ciCandidateSnapshotManifest struct {
	Head             string                           `json:"head"`
	HeadRef          string                           `json:"head_ref"`
	RebaseHeadRef    string                           `json:"rebase_head_ref"`
	IndexTree        string                           `json:"index_tree"`
	Status           string                           `json:"status"`
	TrackedDiff      string                           `json:"tracked_diff"`
	Refs             map[string]ciRefSnapshotManifest `json:"refs"`
	IgnoredPaths     []string                         `json:"ignored_paths"`
	RebaseInProgress bool                             `json:"rebase_in_progress"`
}

func (snapshot ciCandidateSnapshot) persistForPublication() error {
	if snapshot.rootDir == "" {
		return fmt.Errorf("persist CI publication cleanup: missing snapshot root")
	}
	ignoredPaths := make([]string, 0, len(snapshot.ignoredPaths))
	for path := range snapshot.ignoredPaths {
		ignoredPaths = append(ignoredPaths, path)
	}
	sort.Strings(ignoredPaths)
	savedRefs := make(map[string]ciRefSnapshotManifest, len(snapshot.refs))
	for ref, state := range snapshot.refs {
		savedRefs[ref] = ciRefSnapshotManifest{OID: state.oid, Symref: state.symref}
	}
	payload, err := json.Marshal(ciCandidateSnapshotManifest{
		Head: snapshot.head, HeadRef: snapshot.headRef, RebaseHeadRef: snapshot.rebaseHeadRef,
		IndexTree: snapshot.indexTree, Status: snapshot.status, TrackedDiff: snapshot.trackedDiff,
		Refs: savedRefs, IgnoredPaths: ignoredPaths, RebaseInProgress: snapshot.rebaseInProgress,
	})
	if err != nil {
		return fmt.Errorf("encode CI publication cleanup snapshot: %w", err)
	}
	temporary := filepath.Join(snapshot.rootDir, ciPublicationSnapshotManifest+".tmp")
	manifest := filepath.Join(snapshot.rootDir, ciPublicationSnapshotManifest)
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		return fmt.Errorf("write CI publication cleanup snapshot: %w", err)
	}
	if err := os.Rename(temporary, manifest); err != nil {
		return fmt.Errorf("commit CI publication cleanup snapshot: %w", err)
	}
	if err := syncCISnapshotTree(snapshot.rootDir); err != nil {
		return fmt.Errorf("sync CI publication cleanup snapshot: %w", err)
	}
	return nil
}

func validateCIPublicationSnapshotRoot(sctx *pipeline.StepContext, rootDir string) (string, error) {
	gitDir, err := stepGitRun(sctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("resolve Git directory for publication cleanup: %w", err)
	}
	rootDir = filepath.Clean(rootDir)
	gitDir = filepath.Clean(gitDir)
	if filepath.Dir(rootDir) != gitDir || !strings.HasPrefix(filepath.Base(rootDir), "no-mistakes-ci-candidate-") {
		return "", fmt.Errorf("publication cleanup snapshot %q is outside the worktree Git directory", rootDir)
	}
	info, err := os.Lstat(rootDir)
	if err != nil {
		return "", fmt.Errorf("inspect publication cleanup snapshot: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("publication cleanup snapshot %q is not a trusted directory", rootDir)
	}
	canonicalGitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return "", fmt.Errorf("resolve publication Git directory: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return "", fmt.Errorf("resolve publication cleanup snapshot: %w", err)
	}
	if filepath.Dir(canonicalRoot) != canonicalGitDir {
		return "", fmt.Errorf("publication cleanup snapshot %q resolves outside the worktree Git directory", rootDir)
	}
	return rootDir, nil
}

func loadCIPublicationSnapshot(sctx *pipeline.StepContext, rootDir string) (ciCandidateSnapshot, error) {
	rootDir, err := validateCIPublicationSnapshotRoot(sctx, rootDir)
	if err != nil {
		return ciCandidateSnapshot{}, err
	}
	worktreeDir := filepath.Join(rootDir, "worktree")
	rebaseStateDir := filepath.Join(rootDir, "git-state")
	for _, path := range []string{worktreeDir, rebaseStateDir} {
		childInfo, err := os.Lstat(path)
		if err != nil {
			return ciCandidateSnapshot{}, fmt.Errorf("inspect publication cleanup payload: %w", err)
		}
		if childInfo.Mode()&os.ModeSymlink != 0 || !childInfo.IsDir() {
			return ciCandidateSnapshot{}, fmt.Errorf("publication cleanup payload %q is not a trusted directory", path)
		}
	}
	manifestPath := filepath.Join(rootDir, ciPublicationSnapshotManifest)
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("inspect publication cleanup manifest: %w", err)
	}
	if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
		return ciCandidateSnapshot{}, fmt.Errorf("publication cleanup manifest is not a trusted regular file")
	}
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("read publication cleanup manifest: %w", err)
	}
	var saved ciCandidateSnapshotManifest
	if err := json.Unmarshal(payload, &saved); err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("decode publication cleanup manifest: %w", err)
	}
	ignoredPaths := make(map[string]struct{}, len(saved.IgnoredPaths))
	for _, path := range saved.IgnoredPaths {
		clean := filepath.Clean(path)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean != path {
			return ciCandidateSnapshot{}, fmt.Errorf("publication cleanup manifest contains unsafe ignored path %q", path)
		}
		ignoredPaths[path] = struct{}{}
	}
	refs := make(map[string]rebaseRefState, len(saved.Refs))
	for ref, state := range saved.Refs {
		refs[ref] = rebaseRefState{oid: state.OID, symref: state.Symref}
	}
	return ciCandidateSnapshot{
		head: saved.Head, headRef: saved.HeadRef, rebaseHeadRef: saved.RebaseHeadRef,
		indexTree: saved.IndexTree, status: saved.Status, trackedDiff: saved.TrackedDiff,
		rootDir: rootDir, worktreeDir: worktreeDir, rebaseStateDir: rebaseStateDir,
		refs: refs, ignoredPaths: ignoredPaths, rebaseInProgress: saved.RebaseInProgress,
	}, nil
}

func syncCISnapshotTree(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := syncCISnapshotTree(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func (snapshot ciCandidateSnapshot) cleanup(_ *pipeline.StepContext) {
	if snapshot.rootDir != "" {
		if err := os.RemoveAll(snapshot.rootDir); err != nil {
			slog.Warn("failed to remove CI candidate snapshot", "err", err)
		}
		return
	}
	if snapshot.worktreeDir != "" {
		if err := os.RemoveAll(snapshot.worktreeDir); err != nil {
			slog.Warn("failed to remove CI candidate snapshot", "err", err)
		}
	}
	if snapshot.rebaseStateDir != "" {
		if err := os.RemoveAll(snapshot.rebaseStateDir); err != nil {
			slog.Warn("failed to remove CI rebase snapshot", "err", err)
		}
	}
}

type ciJournalError struct {
	operation string
	err       error
}

func (e *ciJournalError) Error() string { return e.operation + ": " + e.err.Error() }
func (e *ciJournalError) Unwrap() error { return e.err }

func isCIJournalFailure(err error) bool {
	var journalErr *ciJournalError
	return errors.As(err, &journalErr)
}

func isCIProfileExhaustion(err error) bool {
	var exhausted *agent.ProfileUnavailableError
	return errors.As(err, &exhausted)
}

type ciPublicationPendingError struct {
	sha string
	err error
}

func (e *ciPublicationPendingError) Error() string {
	return fmt.Sprintf("publish sealed CI candidate %s: %v", shortSHA(e.sha), e.err)
}

func (e *ciPublicationPendingError) Unwrap() error { return e.err }

func isCIPublicationPending(err error) bool {
	var pendingErr *ciPublicationPendingError
	return errors.As(err, &pendingErr)
}

type ciRepairIssue struct {
	LineageID     string
	Name          string
	MergeConflict bool
	Tier          int
}

type ciRepairPlan struct {
	Issues    []ciRepairIssue
	Tier      int
	Exhausted bool
}

func ciHostedFailureLineage(runID, prURL, kind, name string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{runID, prURL, kind, strings.TrimSpace(name)}, "\x00")))
	return fmt.Sprintf("ci:%x", sum[:])
}

func (s *CIStep) planCIRepair(sctx *pipeline.StepContext, pr *scm.PR, failingNames []string, mergeConflict bool, budget int) (ciRepairPlan, error) {
	names := append([]string(nil), failingNames...)
	sort.Strings(names)
	all := make([]ciRepairIssue, 0, len(names)+1)
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		all = append(all, ciRepairIssue{
			LineageID: ciHostedFailureLineage(sctx.Run.ID, pr.URL, "check", name),
			Name:      name,
		})
	}
	if mergeConflict {
		all = append(all, ciRepairIssue{
			LineageID:     ciHostedFailureLineage(sctx.Run.ID, pr.URL, "merge-conflict", ""),
			Name:          "merge conflict",
			MergeConflict: true,
		})
	}
	plan := ciRepairPlan{Tier: -1}
	for i := range all {
		tier, err := s.ciLineageTier(sctx, all[i].LineageID)
		if err != nil {
			return ciRepairPlan{}, err
		}
		all[i].Tier = tier
		if tier >= budget {
			plan.Exhausted = true
			continue
		}
		if plan.Tier == -1 || tier < plan.Tier {
			plan.Tier = tier
			plan.Issues = plan.Issues[:0]
		}
		if tier == plan.Tier {
			plan.Issues = append(plan.Issues, all[i])
		}
	}
	if plan.Tier < 0 {
		plan.Tier = 0
	}
	return plan, nil
}

func (s *CIStep) ciLineageTier(sctx *pipeline.StepContext, lineageID string) (int, error) {
	if sctx.StepResultID != "" && sctx.CurrentRound != nil {
		repairs, err := sctx.DB.GetFindingRepairsByLineage(lineageID)
		if err != nil {
			return 0, &ciJournalError{operation: "load hosted CI repair lineage", err: err}
		}
		tier := 0
		for _, repair := range repairs {
			if repair.Status != db.RepairStatusUnavailable && repair.Tier >= tier {
				tier = repair.Tier + 1
			}
		}
		return tier, nil
	}
	if s.ephemeralCIRepairs == nil {
		s.ephemeralCIRepairs = make(map[string]int)
	}
	return s.ephemeralCIRepairs[lineageID], nil
}

func (s *CIStep) beginCIRepairs(sctx *pipeline.StepContext, plan ciRepairPlan, budget int) ([]string, error) {
	if len(plan.Issues) == 0 {
		return nil, nil
	}
	if sctx.StepResultID == "" || sctx.CurrentRound == nil {
		if s.ephemeralCIRepairs == nil {
			s.ephemeralCIRepairs = make(map[string]int)
		}
		for _, issue := range plan.Issues {
			s.ephemeralCIRepairs[issue.LineageID]++
		}
		return nil, nil
	}
	ids := make([]string, 0, len(plan.Issues))
	for _, issue := range plan.Issues {
		id, err := sctx.DB.StartFindingRepair(db.FindingRepairStart{
			RunID:           sctx.Run.ID,
			LineageID:       issue.LineageID,
			StepResultID:    sctx.StepResultID,
			StepRoundID:     sctx.CurrentRound.ID,
			Severity:        "error",
			Action:          "auto-fix",
			Description:     "hosted CI failure: " + issue.Name,
			Tier:            issue.Tier,
			RemainingBudget: budget - issue.Tier - 1,
		})
		if err != nil {
			return nil, &ciJournalError{operation: "start hosted CI repair", err: err}
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func finishCIRepairs(sctx *pipeline.StepContext, repairIDs []string, verdict, rationale, status string) error {
	for _, id := range repairIDs {
		if err := sctx.DB.ResolveFindingRepair(id, verdict, rationale, status); err != nil {
			return &ciJournalError{operation: "finish hosted CI repair", err: err}
		}
	}
	return nil
}

func ciInvocationAttemptIDs(sctx *pipeline.StepContext, repairIDs []string) (map[string]struct{}, error) {
	if len(repairIDs) == 0 || sctx.CurrentRound == nil {
		return nil, nil
	}
	attempts, err := sctx.DB.GetInvocationAttemptsByRound(sctx.CurrentRound.ID)
	if err != nil {
		return nil, &ciJournalError{operation: "load hosted CI invocation attempts", err: err}
	}
	ids := make(map[string]struct{}, len(attempts))
	for _, attempt := range attempts {
		ids[attempt.ID] = struct{}{}
	}
	return ids, nil
}

func linkCIInvocationAfter(sctx *pipeline.StepContext, repairIDs []string, purpose types.Purpose, fixer bool, priorAttempts map[string]struct{}) (bool, error) {
	if len(repairIDs) == 0 || sctx.CurrentRound == nil {
		return true, nil
	}
	attempts, err := sctx.DB.GetInvocationAttemptsByRound(sctx.CurrentRound.ID)
	if err != nil {
		return false, &ciJournalError{operation: "load hosted CI invocation attempts", err: err}
	}
	attemptID := ""
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].Start.Purpose == purpose {
			if _, alreadyPresent := priorAttempts[attempts[i].ID]; alreadyPresent {
				continue
			}
			attemptID = attempts[i].ID
			break
		}
	}
	if attemptID == "" {
		return false, nil
	}
	for _, repairID := range repairIDs {
		if fixer {
			err = sctx.DB.SetFindingRepairFixer(repairID, attemptID)
		} else {
			err = sctx.DB.SetFindingRepairVerifier(repairID, attemptID)
		}
		if err != nil {
			return false, &ciJournalError{operation: "link hosted CI invocation", err: err}
		}
	}
	return true, nil
}

func (s *CIStep) runPlannedCIRepair(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, plan ciRepairPlan, budget int) (bool, error) {
	s.activeCIRepairTier = plan.Tier
	s.activeCIRepairPlan = plan
	s.activeCIRepairBudget = budget
	s.activeCIRepairIDs = nil
	defer func() {
		s.activeCIRepairTier = 0
		s.activeCIRepairPlan = ciRepairPlan{}
		s.activeCIRepairBudget = 0
		s.activeCIRepairIDs = nil
	}()
	failingNames, mergeConflict := selectedCIRepairIssues(plan)
	pushed, repairErr := s.autoFixCI(sctx, host, pr, failingNames, mergeConflict)
	if repairErr != nil {
		status := db.RepairStatusFailed
		verdict := db.RepairVerdictInconclusive
		if isCIProfileExhaustion(repairErr) {
			status = db.RepairStatusUnavailable
		} else if isCIPublicationPending(repairErr) {
			status = db.RepairStatusUnresolved
			verdict = db.RepairVerdictUnresolved
		}
		if finishErr := finishCIRepairs(sctx, s.activeCIRepairIDs, verdict, repairErr.Error(), status); finishErr != nil {
			return false, fmt.Errorf("%v; additionally failed to journal hosted CI repair: %w", repairErr, finishErr)
		}
		return false, repairErr
	}
	if err := finishCIRepairs(sctx, s.activeCIRepairIDs, db.RepairVerdictUnresolved, "published candidate awaiting hosted CI recheck", db.RepairStatusUnresolved); err != nil {
		return false, err
	}
	return pushed, nil
}

func selectedCIRepairIssues(plan ciRepairPlan) ([]string, bool) {
	names := make([]string, 0, len(plan.Issues))
	mergeConflict := false
	for _, issue := range plan.Issues {
		if issue.MergeConflict {
			mergeConflict = true
		} else {
			names = append(names, issue.Name)
		}
	}
	return names, mergeConflict
}

func resolveHostedCIRepairs(sctx *pipeline.StepContext) error {
	if sctx.StepResultID == "" || sctx.CurrentRound == nil {
		return nil
	}
	repairs, err := sctx.DB.GetFindingRepairsByRun(sctx.Run.ID)
	if err != nil {
		return &ciJournalError{operation: "load hosted CI repairs", err: err}
	}
	latest := make(map[string]*db.FindingRepair)
	for _, repair := range repairs {
		if strings.HasPrefix(repair.LineageID, "ci:") {
			latest[repair.LineageID] = repair
		}
	}
	for _, repair := range latest {
		if repair.Status == db.RepairStatusResolved {
			continue
		}
		if err := sctx.DB.ResolveFindingRepair(repair.ID, db.RepairVerdictResolved, "hosted CI no longer reports the failure", db.RepairStatusResolved); err != nil {
			return &ciJournalError{operation: "resolve hosted CI repair", err: err}
		}
	}
	return nil
}

// ciRepairTier returns the durable hosted-failure tier selected for this
// invocation. Provider failover happens inside that Profile; callers never
// advance this value in response to Profile exhaustion.
func (s *CIStep) ciRepairTier(_ *pipeline.StepContext) int {
	return s.activeCIRepairTier
}

// verifyCIPatch runs the configured local deterministic checks and a fresh
// strong verifier over the uncommitted CI patch. Any failing check, an
// inconclusive verifier, or a blocking finding is returned as an error so the
// caller fails closed without publishing.
func (s *CIStep) verifyCIPatch(sctx *pipeline.StepContext, baseSHA string) error {
	for _, chk := range []struct{ name, cmd string }{
		{"test", sctx.Config.Commands.Test},
		{"lint", sctx.Config.Commands.Lint},
	} {
		if strings.TrimSpace(chk.cmd) == "" {
			continue
		}
		sctx.Log(fmt.Sprintf("running local %s check on CI patch...", chk.name))
		output, exitCode, err := runStepShellCommand(sctx, chk.cmd)
		if err != nil {
			return fmt.Errorf("run %s check: %w", chk.name, err)
		}
		for _, repairID := range s.activeCIRepairIDs {
			if journalErr := sctx.DB.RecordFindingRepairCheck(repairID, chk.cmd, true, exitCode, trimLogOutput(strings.TrimSpace(output), 4096)); journalErr != nil {
				return &ciJournalError{operation: "journal hosted CI deterministic check", err: journalErr}
			}
		}
		if exitCode != 0 {
			return fmt.Errorf("local %s check failed (exit %d)", chk.name, exitCode)
		}
	}

	priorAttempts, priorErr := ciInvocationAttemptIDs(sctx, s.activeCIRepairIDs)
	if priorErr != nil {
		return priorErr
	}
	result, err := sctx.InvokeAgent(types.PurposeEscalatedAggregateVerification, agent.RunOpts{
		Prompt:     buildCIVerifyPrompt(sctx, baseSHA),
		CWD:        sctx.WorkDir,
		JSONSchema: findingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	linked, linkErr := linkCIInvocationAfter(sctx, s.activeCIRepairIDs, types.PurposeEscalatedAggregateVerification, false, priorAttempts)
	if linkErr != nil {
		return linkErr
	}
	if err != nil {
		return fmt.Errorf("strong verifier inconclusive: %w", err)
	}
	if !linked {
		return &ciJournalError{operation: "link hosted CI invocation", err: fmt.Errorf("no journaled %s attempt in current round", types.PurposeEscalatedAggregateVerification)}
	}
	if result == nil || result.Output == nil {
		return fmt.Errorf("strong verifier returned no structured findings")
	}
	findings, err := validateFindingsOutput(result.Output)
	if err != nil {
		return fmt.Errorf("strong verifier returned inconclusive structured findings: %w", err)
	}
	if hasBlockingFindings(findings.Items) {
		return fmt.Errorf("strong verifier rejected the CI patch: %s", findings.Summary)
	}
	return nil
}

func buildCIVerifyPrompt(sctx *pipeline.StepContext, baseSHA string) string {
	prompt := fmt.Sprintf(
		`You are independently verifying a CI-repair patch before it is republished.

Base commit: %s

The candidate changes (a staged worktree patch or a clean changed HEAD) were produced to fix failing CI checks or a merge conflict. Verify the complete candidate:
- Confirm the candidate actually addresses the failure without introducing correctness, security, or data-loss regressions.
- Confirm the change is internally coherent and preserves the intent of the original work.
- Treat inconclusive or unverifiable evidence as a blocking concern rather than a pass.

Return structured findings. Use severity "error" or "warning" for anything that must block republishing, and return an empty findings list only when the patch is fully verified.`,
		baseSHA,
	)
	prompt += userIntentPromptSection(sctx)
	return prompt
}

// commitAndPush commits any uncommitted changes and force-pushes to the
// configured push remote.
// Returns (true, nil) when changes were pushed, (false, nil) when there was
// nothing to commit, or (false, err) on failure.
func (s *CIStep) commitAndPush(sctx *pipeline.StepContext) (bool, error) {
	return s.commitAndPushWithSnapshot(sctx, nil)
}

func (s *CIStep) commitAndPushWithSnapshot(sctx *pipeline.StepContext, cleanupSnapshot *ciCandidateSnapshot) (bool, error) {
	status, err := stepGitRun(sctx, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check CI changes: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		if s.verifiedCandidateTree != "" {
			if err := validatePreparedCICandidate(sctx, s.verifiedCandidateHead, s.verifiedCandidateTree); err != nil {
				return false, fmt.Errorf("verified CI candidate changed before republish: %w", err)
			}
		}
		sctx.Log("no changes to commit")
		headSHA, err := stepGitHeadSHA(sctx)
		if err == nil && headSHA != sctx.Run.HeadSHA {
			return s.pushUpdatedHeadSHAWithSnapshot(sctx, headSHA, cleanupSnapshot)
		}
		return false, nil
	}

	if s.verifiedCandidateTree == "" {
		if _, err := stepGitRun(sctx, "add", "-A"); err != nil {
			return false, fmt.Errorf("stage CI changes: %w", err)
		}
	} else if err := validatePreparedCICandidate(sctx, s.verifiedCandidateHead, s.verifiedCandidateTree); err != nil {
		return false, fmt.Errorf("verified CI candidate changed before commit: %w", err)
	}
	if _, err := stepGitRun(sctx, "-c", "core.hooksPath=/dev/null", "commit", "-m", "no-mistakes: apply CI fixes"); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	headSHA, err := stepGitHeadSHA(sctx)
	if err != nil {
		return false, fmt.Errorf("resolve head after commit: %w", err)
	}
	if s.verifiedCandidateTree != "" {
		tree, err := stepGitRun(sctx, "rev-parse", headSHA+"^{tree}")
		if err != nil {
			return false, fmt.Errorf("resolve committed CI candidate tree: %w", err)
		}
		if tree != s.verifiedCandidateTree {
			return false, fmt.Errorf("committed CI candidate tree %s does not match verified tree %s", shortSHA(tree), shortSHA(s.verifiedCandidateTree))
		}
	}

	return s.pushUpdatedHeadSHAWithSnapshot(sctx, headSHA, cleanupSnapshot)
}

func (s *CIStep) pushUpdatedHeadSHA(sctx *pipeline.StepContext, newHeadSHA string) (bool, error) {
	return s.pushUpdatedHeadSHAWithSnapshot(sctx, newHeadSHA, nil)
}

func (s *CIStep) pushUpdatedHeadSHAWithSnapshot(sctx *pipeline.StepContext, newHeadSHA string, cleanupSnapshot *ciCandidateSnapshot) (bool, error) {
	ref := normalizedBranchRef(sctx.Run.Branch)
	gitRun := func(args ...string) (string, error) { return stepGitRun(sctx, args...) }

	if s.verifiedCandidateTree != "" {
		tree, err := stepGitRun(sctx, "rev-parse", newHeadSHA+"^{tree}")
		if err != nil {
			return false, fmt.Errorf("resolve republish candidate tree: %w", err)
		}
		if tree != s.verifiedCandidateTree {
			return false, fmt.Errorf("republish SHA %s does not name verified tree %s", shortSHA(newHeadSHA), shortSHA(s.verifiedCandidateTree))
		}
	}

	publication, err := sctx.DB.LatestPublication(sctx.Run.ID, db.PublicationKindCI)
	if err != nil {
		return false, &ciJournalError{operation: "load CI publication transaction", err: err}
	}
	if publication != nil && publication.SealSHA != newHeadSHA && publication.State != db.PublicationStateCompleted {
		return false, &ciJournalError{operation: "prepare CI publication", err: fmt.Errorf("incomplete sealed candidate %s blocks newer candidate %s", shortSHA(publication.SealSHA), shortSHA(newHeadSHA))}
	}
	if publication == nil || publication.SealSHA != newHeadSHA {
		pushURL := sctx.Repo.PushURL()
		decision, err := resolveForcePushDecision(gitRun, pushURL, ref, newHeadSHA, sctx.Run.HeadSHA, sctx.Run.BaseSHA)
		if err != nil {
			return false, &ciPublicationPendingError{sha: newHeadSHA, err: err}
		}
		cleanupSnapshotDir := ""
		if cleanupSnapshot != nil {
			if err := cleanupSnapshot.persistForPublication(); err != nil {
				return false, &ciPublicationPendingError{sha: newHeadSHA, err: err}
			}
			cleanupSnapshotDir = cleanupSnapshot.rootDir
		}
		if s.sealCIRepublish != nil {
			if err := s.ensureCIRepublishSeal(sctx, newHeadSHA); err != nil {
				return false, err
			}
			seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
			if err != nil {
				return false, &ciJournalError{operation: "load CI republish seal", err: err}
			}
			publication, err = sctx.DB.PreparePublication(db.PreparePublicationInput{
				RunID:              sctx.Run.ID,
				Kind:               db.PublicationKindCI,
				SealID:             seal.ID,
				SealSHA:            newHeadSHA,
				DestinationURL:     pushURL,
				DestinationRef:     ref,
				ExpectedRemoteSHA:  decision.remoteSHA,
				Force:              !decision.upToDate,
				CleanupSnapshotDir: cleanupSnapshotDir,
			})
		} else {
			_, publication, err = sctx.DB.PrepareCISealedPublication(db.PrepareCISealedPublicationInput{
				RunID:              sctx.Run.ID,
				SHA:                newHeadSHA,
				DestinationURL:     pushURL,
				DestinationRef:     ref,
				ExpectedRemoteSHA:  decision.remoteSHA,
				Force:              !decision.upToDate,
				CleanupSnapshotDir: cleanupSnapshotDir,
			})
		}
		if err != nil {
			return false, &ciJournalError{operation: "prepare durable CI publication", err: err}
		}
	}
	if err := protectCIRepublishCandidate(sctx, newHeadSHA); err != nil {
		return false, &ciPublicationPendingError{sha: newHeadSHA, err: err}
	}

	transport := s.transportPublication
	if transport == nil {
		transport = func(_ context.Context, _, destinationURL, sourceSHA, destinationRef, expectedRemoteSHA string, force bool) error {
			return stepGitPush(sctx, destinationURL, sourceSHA, destinationRef, expectedRemoteSHA, force)
		}
	}

	var cleanup publicationCleanup
	if s.restorePublishedState != nil && publication.CleanupSnapshotDir != "" {
		cleanup = func(sctx *pipeline.StepContext, publication *db.Publication) error {
			snapshot, err := loadCIPublicationSnapshot(sctx, publication.CleanupSnapshotDir)
			if err != nil {
				return err
			}
			return s.restorePublishedState(sctx, snapshot, publication.SealSHA)
		}
	}

	pushed, err := executePreparedPublication(
		sctx,
		publication,
		transport,
		gitRun,
		db.PublicationRecoveryNone,
		cleanup,
	)
	if err != nil {
		return false, &ciPublicationPendingError{sha: newHeadSHA, err: err}
	}
	if err := clearCIRepublishPending(sctx); err != nil {
		return pushed, &ciPublicationPendingError{sha: newHeadSHA, err: err}
	}
	if pushed {
		sctx.Log("committed and pushed fixes")
	}
	return pushed, nil
}

func ciRepublishPendingRef(sctx *pipeline.StepContext) string {
	sum := sha256.Sum256([]byte(sctx.Run.ID))
	return fmt.Sprintf("refs/no-mistakes/ci-republish-pending/%x", sum[:])
}

func protectCIRepublishCandidate(sctx *pipeline.StepContext, sha string) error {
	if _, err := stepGitRun(sctx, "update-ref", ciRepublishPendingRef(sctx), sha); err != nil {
		return fmt.Errorf("protect sealed CI candidate %s: %w", shortSHA(sha), err)
	}
	return nil
}

func clearCIRepublishPending(sctx *pipeline.StepContext) error {
	if _, err := stepGitRun(sctx, "update-ref", "-d", ciRepublishPendingRef(sctx)); err != nil {
		return fmt.Errorf("clear pending CI publication: %w", err)
	}
	return nil
}

func pendingCIRepublishSHA(sctx *pipeline.StepContext) (string, error) {
	sha, err := stepGitRun(sctx, "for-each-ref", "--format=%(objectname)", ciRepublishPendingRef(sctx))
	if err != nil {
		return "", fmt.Errorf("load pending CI publication: %w", err)
	}
	fields := strings.Fields(sha)
	if len(fields) == 0 {
		return "", nil
	}
	if len(fields) != 1 {
		return "", fmt.Errorf("load pending CI publication: expected one protected candidate, got %d", len(fields))
	}
	if _, err := stepGitRun(sctx, "cat-file", "-e", fields[0]+"^{commit}"); err != nil {
		return "", fmt.Errorf("validate pending CI publication %s: %w", shortSHA(fields[0]), err)
	}
	return fields[0], nil
}

func (s *CIStep) retryPendingCIRepublish(sctx *pipeline.StepContext) (bool, error) {
	publication, err := sctx.DB.LatestPublication(sctx.Run.ID, db.PublicationKindCI)
	if err != nil {
		return true, &ciJournalError{operation: "load pending CI publication", err: err}
	}
	if publication != nil && publication.State != db.PublicationStateCompleted {
		sctx.Log(fmt.Sprintf("retrying publication of sealed CI candidate %s...", shortSHA(publication.SealSHA)))
		if _, err := s.pushUpdatedHeadSHA(sctx, publication.SealSHA); err != nil {
			return true, err
		}
		sctx.Log(fmt.Sprintf("published sealed CI candidate %s", shortSHA(publication.SealSHA)))
		return true, nil
	}

	sha, err := pendingCIRepublishSHA(sctx)
	if err != nil {
		return true, err
	}
	if sha == "" {
		seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
		if err != nil {
			return true, &ciJournalError{operation: "load seal-only CI publication", err: err}
		}
		if seal == nil || seal.SHA == sctx.Run.HeadSHA {
			return false, nil
		}
		sha = seal.SHA
	}
	if publication != nil && publication.State == db.PublicationStateCompleted {
		if publication.SealSHA != sha {
			return true, &ciJournalError{operation: "clear completed CI publication", err: fmt.Errorf("protected candidate %s does not match completed publication %s", shortSHA(sha), shortSHA(publication.SealSHA))}
		}
		if publication.CleanupSnapshotDir == "" {
			if err := reconcilePublicationWorktree(sctx, sha); err != nil {
				return true, &ciPublicationPendingError{sha: sha, err: err}
			}
		} else if err := removeCompletedPublicationSnapshot(sctx, publication); err != nil {
			return true, &ciPublicationPendingError{sha: sha, err: err}
		}
		if err := clearCIRepublishPending(sctx); err != nil {
			return true, &ciPublicationPendingError{sha: sha, err: err}
		}
		return true, nil
	}

	sctx.Log(fmt.Sprintf("recovering publication of sealed CI candidate %s...", shortSHA(sha)))
	if _, err := s.pushUpdatedHeadSHA(sctx, sha); err != nil {
		return true, err
	}
	sctx.Log(fmt.Sprintf("published sealed CI candidate %s", shortSHA(sha)))
	return true, nil
}

func (s *CIStep) ensureCIRepublishSeal(sctx *pipeline.StepContext, sha string) error {
	var err error
	if s.sealCIRepublish != nil {
		err = s.sealCIRepublish(sctx, sha)
	} else {
		err = ensureCIRepublishSeal(sctx, sha)
	}
	if err != nil {
		return err
	}
	seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
	if err != nil {
		return &ciJournalError{operation: "confirm CI republish seal", err: err}
	}
	if seal == nil || seal.SHA != sha {
		return &ciJournalError{operation: "confirm CI republish seal", err: fmt.Errorf("exact candidate %s is not durably sealed", shortSHA(sha))}
	}
	return nil
}

func ensureCIRepublishSeal(sctx *pipeline.StepContext, sha string) error {
	seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
	if err != nil {
		return &ciJournalError{operation: "load CI republish seal", err: err}
	}
	if seal != nil && seal.SHA == sha {
		return nil
	}
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, sha, "ci_republish"); err != nil {
		return &ciJournalError{operation: fmt.Sprintf("seal CI republish candidate %s", shortSHA(sha)), err: err}
	}
	return nil
}
