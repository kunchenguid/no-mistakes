package steps

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
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
	status, _ := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		sctx.Log("committing agent changes...")
		stagedPaths, err := git.Run(ctx, sctx.WorkDir, "diff", "--cached", "--name-only")
		if err != nil {
			return nil, fmt.Errorf("inspect staged agent changes: %w", err)
		}
		if strings.TrimSpace(stagedPaths) != "" {
			if _, err := git.Run(ctx, sctx.WorkDir, "commit", "-m", "no-mistakes: apply agent fixes"); err != nil {
				return nil, fmt.Errorf("commit agent changes: %w", err)
			}
			headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
			if err != nil {
				return nil, fmt.Errorf("resolve head after commit: %w", err)
			}
			newHeadSHA = headSHA
		}
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

func (s *PushStep) stageAgentChanges(sctx *pipeline.StepContext) error {
	location := resolveTestEvidenceLocation(sctx.WorkDir, sctx.Run.Branch, sctx.Run.ID, sctx.Config.Test.Evidence)
	if !location.StoreInRepo {
		_, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "-A")
		return err
	}
	managedPaths, err := managedEvidenceDestinationPaths(sctx, location)
	if err != nil {
		return err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "-u"); err != nil {
		return err
	}
	untracked, err := git.Run(sctx.Ctx, sctx.WorkDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for _, rel := range strings.Split(untracked, "\x00") {
		if rel == "" {
			continue
		}
		if managedPaths[filepath.ToSlash(rel)] {
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
		if result.StepName != types.StepTest || result.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*result.FindingsJSON)
		if err != nil {
			continue
		}
		for _, artifact := range findings.Artifacts {
			if rel, ok := preparedEvidenceDestinationPath(sctx.WorkDir, location.RepoDir, artifact); ok {
				managed[rel] = true
			}
		}
	}
	return managed, nil
}

func preparedEvidenceDestinationPath(workDir, repoDir string, artifact types.TestArtifact) (string, bool) {
	if !isImageArtifact(artifact.Kind, artifact.Path) || artifact.URL != "" || filepath.IsAbs(artifact.Path) ||
		len(artifact.SHA256) != sha256.Size*2 || artifact.SHA256 != strings.ToLower(artifact.SHA256) {
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
	if filepath.Base(target) != artifact.SHA256[:32]+".png" {
		return "", false
	}
	return rel, true
}

func (s *PushStep) stageInRepoEvidence(sctx *pipeline.StepContext) error {
	ctx := sctx.Ctx
	location := resolveTestEvidenceLocation(sctx.WorkDir, sctx.Run.Branch, sctx.Run.ID, sctx.Config.Test.Evidence)
	if !location.StoreInRepo {
		return nil
	}
	evidenceRemote := sctx.Repo.UpstreamURL
	if strings.TrimSpace(sctx.Repo.ForkURL) != "" {
		evidenceRemote = sctx.Repo.ForkURL
	}
	_, githubRemote := githubRepositoryForRemote(ctx, evidenceRemote)

	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("load test evidence manifest: %w", err)
	}
	staged := make(map[string]bool)
	for _, result := range steps {
		if result.StepName != types.StepTest || result.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*result.FindingsJSON)
		if err != nil {
			continue
		}
		manifestChanged := false
		for i := range findings.Artifacts {
			artifact := findings.Artifacts[i]
			if !isImageArtifact(artifact.Kind, artifact.Path) || artifact.URL != "" || filepath.IsAbs(artifact.Path) {
				continue
			}
			artifact.Published = false
			target := filepath.Join(sctx.WorkDir, filepath.FromSlash(artifact.Path))
			if _, ok := artifactPathRelativeToRoot(target, location.RepoDir); !ok {
				findings.Artifacts[i] = unpublishedImageArtifact(artifact)
				manifestChanged = true
				continue
			}
			targetRel, ok := artifactPathRelativeToRoot(target, sctx.WorkDir)
			if !ok {
				findings.Artifacts[i] = unpublishedImageArtifact(artifact)
				manifestChanged = true
				continue
			}
			targetRel = filepath.ToSlash(targetRel)
			if _, err := git.Run(ctx, sctx.WorkDir, "reset", "--quiet", "HEAD", "--", targetRel); err != nil {
				return fmt.Errorf("clear staged test evidence: %w", err)
			}
			if !githubRemote || !matchesPreparedEvidenceManifest(target, artifact) {
				findings.Artifacts[i] = unpublishedImageArtifact(artifact)
				manifestChanged = true
				continue
			}
			if staged[targetRel] {
				artifact.Published = true
				findings.Artifacts[i] = artifact
				manifestChanged = true
				continue
			}
			if _, err := git.Run(ctx, sctx.WorkDir, "add", "-f", "--", targetRel); err != nil {
				return fmt.Errorf("stage test evidence: %w", err)
			}
			staged[targetRel] = true
			artifact.Published = true
			findings.Artifacts[i] = artifact
			manifestChanged = true
		}
		if manifestChanged {
			raw, err := types.MarshalFindingsJSON(findings)
			if err != nil {
				return fmt.Errorf("encode test evidence manifest: %w", err)
			}
			if err := sctx.DB.SetStepFindings(result.ID, raw); err != nil {
				return fmt.Errorf("update test evidence manifest: %w", err)
			}
		}
	}
	return nil
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
