package steps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PushStep force-pushes the worktree state to the configured push remote.
type PushStep struct{}

func (s *PushStep) Name() types.StepName { return types.StepPush }

func (s *PushStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	newHeadSHA := ""
	if err := sctx.DB.SetRunPushActive(sctx.Run.ID, true); err != nil {
		return nil, err
	}
	defer func() { _ = sctx.DB.SetRunPushActive(sctx.Run.ID, false) }()

	// Run format command if configured (before committing, so changes are formatted)
	if fmtCmd := sctx.Config.Commands.Format; fmtCmd != "" {
		sctx.Log(fmt.Sprintf("running formatter: %s", fmtCmd))
		output, exitCode, err := runStepShellCommand(sctx, fmtCmd)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: format command failed: %v", err))
		} else if exitCode != 0 {
			sctx.Log(fmt.Sprintf("warning: format command exited with code %d: %s", exitCode, output))
		}
	}

	// Commit any uncommitted changes from agent fixes
	if err := s.stageAgentChanges(sctx); err != nil {
		return nil, fmt.Errorf("stage agent changes: %w", err)
	}
	if err := s.stageInRepoEvidence(sctx); err != nil {
		return nil, err
	}
	hasStagedChanges, err := hasStagedGitChanges(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("inspect staged agent changes: %w", err)
	}
	if hasStagedChanges {
		sctx.Log("committing agent changes...")
		if _, err := git.Run(ctx, sctx.WorkDir, "commit", "-m", "no-mistakes: apply agent fixes"); err != nil {
			return nil, fmt.Errorf("commit agent changes: %w", err)
		}
		headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("resolve head after commit: %w", err)
		}
		newHeadSHA = headSHA
	}
	committedHead, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve head for test evidence verification: %w", err)
	}
	if err := s.verifyCommittedInRepoEvidence(sctx, committedHead); err != nil {
		return nil, err
	}

	ref := normalizedBranchRef(sctx.Run.Branch)
	branch := strings.TrimPrefix(ref, "refs/heads/")

	pushURL := resolvePushURL(sctx)
	pushTarget := "upstream"
	usingFork := strings.TrimSpace(sctx.Repo.ForkURL) != ""
	if usingFork {
		pushTarget = "fork"
		sctx.Log(fmt.Sprintf("pushing to fork %s (%s)...", safeurl.Redact(pushURL), ref))
	} else {
		sctx.Log(fmt.Sprintf("pushing to %s (%s)...", safeurl.Redact(pushURL), ref))
	}

	headBeingPushed, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve head before push: %w", err)
	}

	// Decide whether force-pushing would discard commits the pipeline never saw.
	// The lease is anchored to the remote-tracking ref the rebase step freshly
	// fetched (the exact commit this branch was rebased against), so a push that
	// would clobber an out-of-band or stale-mirror commit fails loudly instead
	// of silently dropping it. A bare --force-with-lease offers no protection
	// when pushing to a URL (no remote-tracking refs), so the anchor is explicit.
	lastSeen := lastFetchedBranchTip(ctx, sctx.WorkDir, branch, usingFork)
	gitRun := func(args ...string) (string, error) { return git.Run(ctx, sctx.WorkDir, args...) }
	decision, err := resolveForcePushDecision(gitRun, pushURL, ref, headBeingPushed, lastSeen, sctx.Run.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("push to %s: %w", pushTarget, err)
	}
	switch {
	case decision.newBranch:
		// New branch: regular push (no force needed).
		if err := git.Push(ctx, sctx.WorkDir, pushURL, ref, "", false); err != nil {
			return nil, fmt.Errorf("push to %s: %w", pushTarget, err)
		}
	case decision.upToDate:
		// Remote already at this exact head. This freshly verified equality is a
		// successful binding even though no objects needed to move.
	default:
		// Existing branch: force-with-lease anchored to the verified remote head.
		if err := git.Push(ctx, sctx.WorkDir, pushURL, ref, decision.remoteSHA, true); err != nil {
			return nil, fmt.Errorf("push to %s: %w", pushTarget, err)
		}
	}
	verifiedRemote, err := git.LsRemote(ctx, sctx.WorkDir, pushURL, ref)
	if err != nil || verifiedRemote != headBeingPushed {
		if err != nil {
			return nil, fmt.Errorf("verify successful push to %s: %w", pushTarget, err)
		}
		return nil, fmt.Errorf("verify successful push to %s: remote head %s does not equal pushed head %s", pushTarget, verifiedRemote, headBeingPushed)
	}
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{
		HeadSHA:           headBeingPushed,
		TargetKind:        pushTarget,
		TargetFingerprint: branchsync.TargetFingerprint(pushURL),
		Ref:               ref,
	}); err != nil {
		return nil, err
	}

	if newHeadSHA != "" {
		if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, newHeadSHA); err != nil {
			return nil, fmt.Errorf("update local branch ref: %w", err)
		}
	}

	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD after push: %w", err)
	}
	if headSHA != sctx.Run.HeadSHA {
		sctx.Run.HeadSHA = headSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
			return nil, err
		}
	}

	sctx.Log("pushed successfully")
	return &pipeline.StepOutcome{}, nil
}

func hasStagedGitChanges(ctx context.Context, workDir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet", "--no-ext-diff")
	cmd.Dir = workDir
	cmd.Env = git.NonInteractiveEnv(workDir)
	shellenv.ConfigureShellCommand(cmd)
	err := shellenv.RunShellCommand(cmd)
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

func (s *PushStep) stageAgentChanges(sctx *pipeline.StepContext) error {
	location := resolveTestEvidenceLocation(sctx.WorkDir, sctx.Run.Branch, sctx.Run.ID, sctx.Config.Test.Evidence)
	if location.GeneratedRepoDir == "" {
		_, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "-A")
		return err
	}
	managedPaths := make(map[string]bool)
	if location.StoreInRepo {
		var err error
		managedPaths, err = managedEvidenceDestinationPaths(sctx, location)
		if err != nil {
			return err
		}
	}
	generatedRel, ok := lexicalRelativePath(sctx.WorkDir, location.GeneratedRepoDir)
	if !ok {
		return fmt.Errorf("resolve generated evidence namespace")
	}
	managedPaths[filepath.ToSlash(generatedRel)] = true
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "-u"); err != nil {
		return err
	}
	for targetRel := range managedPaths {
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "reset", "--quiet", "HEAD", "--", targetRel); err != nil {
			return fmt.Errorf("clear staged test evidence: %w", err)
		}
	}
	untracked, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for _, rawRel := range bytes.Split(untracked, []byte{0}) {
		if len(rawRel) == 0 {
			continue
		}
		rel := string(rawRel)
		if isManagedEvidencePath(filepath.ToSlash(rel), managedPaths) {
			continue
		}
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "--", rel); err != nil {
			return err
		}
	}
	return nil
}

func managedEvidenceDestinationPaths(sctx *pipeline.StepContext, location testEvidenceLocation) (map[string]bool, error) {
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("load test evidence manifest: %w", err)
	}
	managed := make(map[string]bool)
	for _, result := range steps {
		if result.StepName != types.StepTest {
			continue
		}
		addManagedEvidenceDestinationPaths(managed, sctx.WorkDir, location.RepoDir, result.FindingsJSON)
		rounds, err := sctx.DB.GetRoundsByStep(result.ID)
		if err != nil {
			return nil, fmt.Errorf("load test evidence rounds: %w", err)
		}
		for _, round := range rounds {
			addManagedEvidenceDestinationPaths(managed, sctx.WorkDir, location.RepoDir, round.FindingsJSON)
		}
	}
	return managed, nil
}

func addManagedEvidenceDestinationPaths(managed map[string]bool, workDir, repoDir string, raw *string) {
	if raw == nil {
		return
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return
	}
	for _, artifact := range findings.Artifacts {
		if rel, ok := reservedEvidenceDestinationPath(workDir, repoDir, artifact); ok {
			managed[rel] = true
		}
	}
}

func isManagedEvidencePath(rel string, managed map[string]bool) bool {
	if managed[rel] {
		return true
	}
	for destination := range managed {
		if strings.HasPrefix(rel, destination+"/") {
			return true
		}
	}
	return false
}

func reservedEvidenceDestinationPath(workDir, repoDir string, artifact types.TestArtifact) (string, bool) {
	if !isImageArtifact(artifact.Kind, artifact.Path) || artifact.URL != "" || filepath.IsAbs(artifact.Path) ||
		len(artifact.SHA256) != sha256.Size*2 || artifact.SHA256 != strings.ToLower(artifact.SHA256) {
		return "", false
	}
	workAbs, err := filepath.Abs(workDir)
	if err != nil {
		return "", false
	}
	repoAbs, err := filepath.Abs(repoDir)
	if err != nil {
		return "", false
	}
	targetAbs, err := filepath.Abs(filepath.Join(workDir, filepath.FromSlash(artifact.Path)))
	if err != nil {
		return "", false
	}
	workRel, ok := lexicalRelativePath(workAbs, targetAbs)
	if !ok {
		return "", false
	}
	if _, ok := lexicalRelativePath(repoAbs, targetAbs); !ok {
		return "", false
	}
	if filepath.Base(targetAbs) != artifact.SHA256[:32]+".png" {
		return "", false
	}
	return filepath.ToSlash(workRel), true
}

func lexicalRelativePath(root, target string) (string, bool) {
	if !sameVolume(root, target) {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func preparedEvidenceDestinationPath(workDir, repoDir string, artifact types.TestArtifact) (string, bool) {
	rel, ok := reservedEvidenceDestinationPath(workDir, repoDir, artifact)
	if !ok {
		return "", false
	}
	target := filepath.Join(workDir, filepath.FromSlash(rel))
	if _, ok := artifactPathRelativeToRoot(target, repoDir); !ok {
		return "", false
	}
	if _, ok := artifactPathRelativeToRoot(target, workDir); !ok {
		return "", false
	}
	return rel, true
}

func evidenceDestinationPath(workDir, repoDir string, artifact types.TestArtifact) (string, bool) {
	if !isImageArtifact(artifact.Kind, artifact.Path) || artifact.URL != "" || filepath.IsAbs(artifact.Path) {
		return "", false
	}
	target := filepath.Join(workDir, filepath.FromSlash(artifact.Path))
	if _, ok := artifactPathRelativeToRoot(target, repoDir); !ok {
		return "", false
	}
	rel, ok := artifactPathRelativeToRoot(target, workDir)
	if !ok {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	return rel, true
}

func (s *PushStep) stageInRepoEvidence(sctx *pipeline.StepContext) error {
	ctx := sctx.Ctx
	location := resolveTestEvidenceLocation(sctx.WorkDir, sctx.Run.Branch, sctx.Run.ID, sctx.Config.Test.Evidence)
	if !location.StoreInRepo {
		return nil
	}
	managedPaths, err := managedEvidenceDestinationPaths(sctx, location)
	if err != nil {
		return err
	}
	if location.GeneratedRepoDir == "" {
		return nil
	}
	generatedRel, ok := lexicalRelativePath(sctx.WorkDir, location.GeneratedRepoDir)
	if !ok {
		return fmt.Errorf("resolve generated evidence namespace")
	}
	managedPaths[filepath.ToSlash(generatedRel)] = true
	evidenceRemote := sctx.Repo.UpstreamURL
	if strings.TrimSpace(sctx.Repo.ForkURL) != "" {
		evidenceRemote = sctx.Repo.ForkURL
	}
	_, githubRemote := githubRepositoryForRemote(ctx, evidenceRemote)

	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("load test evidence manifest: %w", err)
	}
	type manifestRecord struct {
		id       string
		findings types.Findings
	}
	type destinationRecord struct {
		artifact   types.TestArtifact
		consistent bool
		published  bool
	}
	var manifests []manifestRecord
	destinations := make(map[string]destinationRecord)
	for _, result := range steps {
		if result.StepName != types.StepTest || result.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*result.FindingsJSON)
		if err != nil {
			continue
		}
		manifests = append(manifests, manifestRecord{id: result.ID, findings: findings})
		for _, artifact := range findings.Artifacts {
			targetRel, ok := evidenceDestinationPath(sctx.WorkDir, location.RepoDir, artifact)
			if !ok {
				continue
			}
			destination, exists := destinations[targetRel]
			if !exists {
				destinations[targetRel] = destinationRecord{artifact: artifact, consistent: true}
				continue
			}
			if artifact.SHA256 != destination.artifact.SHA256 || artifact.Size != destination.artifact.Size {
				destination.consistent = false
				destinations[targetRel] = destination
			}
		}
	}

	for targetRel := range managedPaths {
		if _, err := git.Run(ctx, sctx.WorkDir, "reset", "--quiet", "HEAD", "--", targetRel); err != nil {
			return fmt.Errorf("clear staged test evidence: %w", err)
		}
	}
	for targetRel, destination := range destinations {
		target := filepath.Join(sctx.WorkDir, filepath.FromSlash(targetRel))
		preparedRel, prepared := preparedEvidenceDestinationPath(sctx.WorkDir, location.RepoDir, destination.artifact)
		if !githubRemote || !destination.consistent || !prepared || preparedRel != targetRel ||
			!matchesPreparedEvidenceManifest(target, destination.artifact) {
			continue
		}
		if _, err := git.Run(ctx, sctx.WorkDir, "add", "-f", "--", targetRel); err != nil {
			return fmt.Errorf("stage test evidence: %w", err)
		}
		if !matchesStagedEvidenceManifest(ctx, sctx.WorkDir, targetRel, destination.artifact) {
			if _, err := git.Run(ctx, sctx.WorkDir, "reset", "--quiet", "HEAD", "--", targetRel); err != nil {
				return fmt.Errorf("clear invalid staged test evidence: %w", err)
			}
			continue
		}
		destination.published = true
		destinations[targetRel] = destination
	}

	for _, manifest := range manifests {
		manifestChanged := false
		for i, artifact := range manifest.findings.Artifacts {
			if !isImageArtifact(artifact.Kind, artifact.Path) || artifact.URL != "" || filepath.IsAbs(artifact.Path) {
				continue
			}
			targetRel, ok := evidenceDestinationPath(sctx.WorkDir, location.RepoDir, artifact)
			if !ok {
				manifest.findings.Artifacts[i] = retryableUnpublishedImageArtifact(artifact)
				manifestChanged = true
				continue
			}
			destination := destinations[targetRel]
			if !destination.published || artifact.SHA256 != destination.artifact.SHA256 || artifact.Size != destination.artifact.Size {
				manifest.findings.Artifacts[i] = retryableUnpublishedImageArtifact(artifact)
				manifestChanged = true
				continue
			}
			artifact.Published = true
			manifest.findings.Artifacts[i] = artifact
			manifestChanged = true
		}
		if manifestChanged {
			raw, err := types.MarshalFindingsJSON(manifest.findings)
			if err != nil {
				return fmt.Errorf("encode test evidence manifest: %w", err)
			}
			if err := sctx.DB.SetStepFindings(manifest.id, raw); err != nil {
				return fmt.Errorf("update test evidence manifest: %w", err)
			}
		}
	}
	return nil
}

func (s *PushStep) verifyCommittedInRepoEvidence(sctx *pipeline.StepContext, headSHA string) error {
	location := resolveTestEvidenceLocation(sctx.WorkDir, sctx.Run.Branch, sctx.Run.ID, sctx.Config.Test.Evidence)
	if !location.StoreInRepo {
		return nil
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("load committed test evidence manifest: %w", err)
	}
	invalid := false
	for _, result := range steps {
		if result.StepName != types.StepTest || result.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*result.FindingsJSON)
		if err != nil {
			continue
		}
		changed := false
		for i, artifact := range findings.Artifacts {
			if !artifact.Published {
				continue
			}
			repoPath, ok := preparedEvidenceDestinationPath(sctx.WorkDir, location.RepoDir, artifact)
			if ok && matchesGitEvidenceBlob(sctx.Ctx, sctx.WorkDir, headSHA+":"+filepath.ToSlash(repoPath), artifact) {
				continue
			}
			findings.Artifacts[i] = retryableUnpublishedImageArtifact(artifact)
			changed = true
			invalid = true
		}
		if !changed {
			continue
		}
		raw, err := types.MarshalFindingsJSON(findings)
		if err != nil {
			return fmt.Errorf("encode committed test evidence manifest: %w", err)
		}
		if err := sctx.DB.SetStepFindings(result.ID, raw); err != nil {
			return fmt.Errorf("update committed test evidence manifest: %w", err)
		}
	}
	if invalid {
		return fmt.Errorf("verify committed test evidence: commit does not match prepared manifest")
	}
	return nil
}

func retryableUnpublishedImageArtifact(artifact types.TestArtifact) types.TestArtifact {
	artifact.URL = ""
	artifact.Content = unpublishedImageExplanation
	artifact.Published = false
	return artifact
}

func matchesPreparedEvidenceManifest(target string, artifact types.TestArtifact) bool {
	if artifact.Size <= 0 || artifact.Size > maxPublishedImageBytes || len(artifact.SHA256) != sha256.Size*2 || artifact.SHA256 != strings.ToLower(artifact.SHA256) {
		return false
	}
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != artifact.Size {
		return false
	}
	file, err := os.Open(target)
	if err != nil {
		return false
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() != artifact.Size || !os.SameFile(info, openedInfo) {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(file, artifact.Size+1))
	if err != nil || int64(len(data)) != artifact.Size {
		return false
	}
	ext, ok := supportedImageExtension(filepath.Ext(target), data)
	if !ok {
		return false
	}
	sum := sha256.Sum256(data)
	actualHash := fmt.Sprintf("%x", sum[:])
	if actualHash != artifact.SHA256 {
		return false
	}
	return filepath.Base(target) == actualHash[:32]+ext
}
