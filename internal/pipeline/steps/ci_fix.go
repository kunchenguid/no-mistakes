package steps

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	defer func() {
		rollbackContext := *sctx
		rollbackContext.Ctx = context.WithoutCancel(sctx.Ctx)
		if retErr != nil {
			s.verifiedCandidateHead = ""
			s.verifiedCandidateTree = ""
			if restoreErr := candidateBeforeRepair.restore(&rollbackContext); restoreErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("restore candidate after failed CI repair: %w", restoreErr))
			}
		}
		candidateBeforeRepair.cleanup(&rollbackContext)
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

	candidateChanged, candidateHead, candidateTree, err := s.prepareCICandidate(sctx, mergeConflict, rebaseBaseSHA)
	if err != nil {
		return false, err
	}
	if !candidateChanged {
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
	s.verifiedCandidateHead = candidateHead
	s.verifiedCandidateTree = candidateTree
	return s.commitAndPush(sctx)
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
	indexTree        string
	status           string
	trackedDiff      string
	worktreeDir      string
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
	snapshot.worktreeDir, err = os.MkdirTemp("", "no-mistakes-ci-candidate-*")
	if err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("create worktree snapshot: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			snapshot.cleanup(sctx)
		}
	}()
	if err := copyCIWorktree(sctx.WorkDir, snapshot.worktreeDir); err != nil {
		return ciCandidateSnapshot{}, fmt.Errorf("snapshot candidate worktree: %w", err)
	}
	complete = true
	return snapshot, nil
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
	return snapshot.validate(sctx)
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

func (snapshot ciCandidateSnapshot) cleanup(_ *pipeline.StepContext) {
	if snapshot.worktreeDir != "" {
		if err := os.RemoveAll(snapshot.worktreeDir); err != nil {
			slog.Warn("failed to remove CI candidate snapshot", "err", err)
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
			return s.pushUpdatedHeadSHA(sctx, headSHA)
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
	if _, err := stepGitRun(sctx, "commit", "-m", "no-mistakes: apply CI fixes"); err != nil {
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

	return s.pushUpdatedHeadSHA(sctx, headSHA)
}

func (s *CIStep) pushUpdatedHeadSHA(sctx *pipeline.StepContext, newHeadSHA string) (bool, error) {
	ref := normalizedBranchRef(sctx.Run.Branch)
	pushURL := sctx.Repo.PushURL()

	if s.verifiedCandidateTree != "" {
		tree, err := stepGitRun(sctx, "rev-parse", newHeadSHA+"^{tree}")
		if err != nil {
			return false, fmt.Errorf("resolve republish candidate tree: %w", err)
		}
		if tree != s.verifiedCandidateTree {
			return false, fmt.Errorf("republish SHA %s does not name verified tree %s", shortSHA(newHeadSHA), shortSHA(s.verifiedCandidateTree))
		}
	}
	if err := s.ensureCIRepublishSeal(sctx, newHeadSHA); err != nil {
		return false, err
	}
	if err := protectCIRepublishCandidate(sctx, newHeadSHA); err != nil {
		return false, err
	}
	pendingError := func(err error) error {
		return &ciPublicationPendingError{sha: newHeadSHA, err: err}
	}

	// Anchor the force-with-lease to the head the run last recorded for this
	// branch (what the pipeline last pushed/observed), NOT to a SHA freshly read
	// from the remote a moment before pushing. The durable pending ref above
	// protects the sealed commit while this transport decision or push retries.
	gitRun := func(args ...string) (string, error) { return stepGitRun(sctx, args...) }
	decision, err := resolveForcePushDecision(gitRun, pushURL, ref, newHeadSHA, sctx.Run.HeadSHA, sctx.Run.BaseSHA)
	if err != nil {
		return false, pendingError(err)
	}
	pushed := false
	if !decision.upToDate {
		if err := stepGitPush(sctx, pushURL, newHeadSHA, ref, decision.remoteSHA, !decision.newBranch); err != nil {
			return false, pendingError(fmt.Errorf("push: %w", err))
		}
		pushed = true
	}

	if _, err := stepGitRun(sctx, "update-ref", ref, newHeadSHA); err != nil {
		return false, pendingError(fmt.Errorf("update local branch ref: %w", err))
	}
	sctx.Run.HeadSHA = newHeadSHA
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
		return false, pendingError(err)
	}
	if err := clearCIRepublishPending(sctx); err != nil {
		return false, pendingError(err)
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
	sha, err := pendingCIRepublishSHA(sctx)
	if err != nil {
		return true, err
	}
	if sha == "" {
		return false, nil
	}
	seal, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
	if err != nil {
		return true, &ciJournalError{operation: "load pending CI republish seal", err: err}
	}
	if seal == nil {
		if err := s.ensureCIRepublishSeal(sctx, sha); err != nil {
			return true, err
		}
		seal, err = sctx.DB.LatestSealByReason(sctx.Run.ID, "ci_republish")
		if err != nil {
			return true, &ciJournalError{operation: "confirm repaired CI republish seal", err: err}
		}
	}
	if seal == nil || seal.SHA != sha {
		return true, &ciJournalError{operation: "load pending CI republish seal", err: fmt.Errorf("protected candidate %s is not the latest durable seal", shortSHA(sha))}
	}

	sctx.Log(fmt.Sprintf("retrying publication of sealed CI candidate %s...", shortSHA(sha)))
	if _, err := s.pushUpdatedHeadSHA(sctx, sha); err != nil {
		return true, err
	}
	if _, err := stepGitRun(sctx, "reset", "--hard", sha); err != nil {
		if protectErr := protectCIRepublishCandidate(sctx, sha); protectErr != nil {
			err = errors.Join(err, protectErr)
		}
		return true, &ciPublicationPendingError{sha: sha, err: fmt.Errorf("reconcile worktree after publication: %w", err)}
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
