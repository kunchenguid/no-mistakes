package steps

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	if !location.StoreInRepo || location.GeneratedRepoDir == "" {
		_, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "-A")
		return err
	}
	if _, err := validateEvidenceNamespaceOwnership(sctx, location); err != nil {
		return err
	}
	managedPaths := make(map[string]bool)
	generatedRel, ok := lexicalRelativePath(sctx.WorkDir, location.GeneratedRepoDir)
	if !ok {
		return fmt.Errorf("resolve generated evidence namespace")
	}
	managedPaths[filepath.ToSlash(generatedRel)] = true
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "-u"); err != nil {
		return err
	}
	for targetRel := range managedPaths {
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "--literal-pathspecs", "reset", "--quiet", "HEAD", "--", targetRel); err != nil {
			return fmt.Errorf("clear staged test evidence: %w", err)
		}
	}
	const maxPathspecBatchBytes = 256 * 1024
	batch := make([]byte, 0, maxPathspecBatchBytes)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := git.RunRawInput(sctx.Ctx, sctx.WorkDir, batch, "--literal-pathspecs", "add", "--pathspec-from-file=-", "--pathspec-file-nul")
		batch = batch[:0]
		return err
	}
	err := git.StreamRaw(sctx.Ctx, sctx.WorkDir, func(stdout io.Reader) error {
		reader := bufio.NewReader(stdout)
		for {
			rawRel, readErr := reader.ReadBytes(0)
			if len(rawRel) > 0 {
				if rawRel[len(rawRel)-1] != 0 {
					return fmt.Errorf("unterminated untracked path")
				}
				rawRel = rawRel[:len(rawRel)-1]
				rel := string(rawRel)
				if !isManagedEvidencePath(filepath.ToSlash(rel), managedPaths) {
					if len(batch) > 0 && len(batch)+len(rawRel)+1 > maxPathspecBatchBytes {
						if err := flush(); err != nil {
							return err
						}
					}
					batch = append(batch, rawRel...)
					batch = append(batch, 0)
					if len(batch) >= maxPathspecBatchBytes {
						if err := flush(); err != nil {
							return err
						}
					}
				}
			}
			if readErr == io.EOF {
				return flush()
			}
			if readErr != nil {
				return readErr
			}
		}
	}, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	return nil
}

type generatedEvidenceManifest struct {
	Version int                             `json:"version"`
	Files   []generatedEvidenceManifestFile `json:"files"`
}

type generatedEvidenceManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func generatedEvidenceManifestPath(workDir string) (string, string, bool) {
	rel := filepath.ToSlash(filepath.Join(fixedEvidenceRepoDir, generatedEvidenceDir, generatedEvidenceManifestName))
	target := filepath.Join(workDir, filepath.FromSlash(rel))
	if _, ok := lexicalRelativePath(workDir, target); !ok {
		return "", "", false
	}
	return rel, target, true
}

func validateEvidenceNamespaceOwnership(sctx *pipeline.StepContext, location testEvidenceLocation) (generatedEvidenceManifest, error) {
	baseManifest, baseExists, err := loadGeneratedEvidenceManifestAtRef(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, location)
	if err != nil {
		return generatedEvidenceManifest{}, fmt.Errorf("generated evidence namespace is not tool-owned at trusted base: %w", err)
	}
	headManifest, headExists, err := loadGeneratedEvidenceManifestAtRef(sctx.Ctx, sctx.WorkDir, "HEAD", location)
	if err != nil {
		return generatedEvidenceManifest{}, fmt.Errorf("generated evidence namespace is not tool-owned at HEAD: %w", err)
	}
	if headExists {
		return headManifest, nil
	}
	if baseExists {
		return baseManifest, nil
	}
	return generatedEvidenceManifest{Version: 1}, nil
}

func loadGeneratedEvidenceManifestAtRef(ctx context.Context, workDir, ref string, location testEvidenceLocation) (generatedEvidenceManifest, bool, error) {
	if strings.TrimSpace(ref) == "" {
		return generatedEvidenceManifest{}, false, nil
	}
	generatedRel, ok := lexicalRelativePath(workDir, location.GeneratedRepoDir)
	if !ok {
		return generatedEvidenceManifest{}, false, fmt.Errorf("resolve generated namespace")
	}
	generatedRel = filepath.ToSlash(generatedRel)
	var paths []string
	pathBytes := 0
	err := git.StreamRaw(ctx, workDir, func(stdout io.Reader) error {
		reader := bufio.NewReader(stdout)
		for {
			raw, readErr := reader.ReadBytes(0)
			if len(raw) > 0 {
				if raw[len(raw)-1] != 0 {
					return fmt.Errorf("unterminated generated namespace path")
				}
				raw = raw[:len(raw)-1]
				pathBytes += len(raw)
				if len(paths) > maxPublishedImagesPerRun || pathBytes > 64*1024 {
					return fmt.Errorf("generated namespace exceeds manifest bounds")
				}
				paths = append(paths, string(raw))
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}, "ls-tree", "-r", "-z", "--name-only", ref, "--", generatedRel)
	if err != nil {
		return generatedEvidenceManifest{}, false, err
	}
	if len(paths) == 0 {
		return generatedEvidenceManifest{}, false, nil
	}
	manifestRel, _, ok := generatedEvidenceManifestPath(workDir)
	if !ok {
		return generatedEvidenceManifest{}, false, fmt.Errorf("resolve generated evidence manifest")
	}
	raw, err := git.RunRaw(ctx, workDir, "show", ref+":"+manifestRel)
	if err != nil {
		return generatedEvidenceManifest{}, false, fmt.Errorf("missing tool manifest")
	}
	var manifest generatedEvidenceManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return generatedEvidenceManifest{}, false, fmt.Errorf("decode tool manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || manifest.Version != 1 || len(manifest.Files) > maxPublishedImagesPerRun {
		return generatedEvidenceManifest{}, false, fmt.Errorf("invalid tool manifest")
	}
	expected := map[string]bool{manifestRel: true}
	for _, file := range manifest.Files {
		if expected[file.Path] || !validGeneratedEvidenceManifestFile(workDir, location.GeneratedRepoDir, file) {
			return generatedEvidenceManifest{}, false, fmt.Errorf("invalid tool manifest entry")
		}
		expected[file.Path] = true
		artifact := types.TestArtifact{Path: file.Path, SHA256: file.SHA256, Size: file.Size}
		if !matchesGitEvidenceBlob(ctx, workDir, ref+":"+file.Path, artifact) {
			return generatedEvidenceManifest{}, false, fmt.Errorf("tool manifest blob mismatch")
		}
	}
	if len(paths) != len(expected) {
		return generatedEvidenceManifest{}, false, fmt.Errorf("unowned files in generated namespace")
	}
	for _, rel := range paths {
		if !expected[filepath.ToSlash(rel)] {
			return generatedEvidenceManifest{}, false, fmt.Errorf("unowned file in generated namespace")
		}
	}
	return manifest, true, nil
}

func validGeneratedEvidenceManifestFile(workDir, generatedDir string, file generatedEvidenceManifestFile) bool {
	if file.Path == "" || filepath.IsAbs(file.Path) || strings.Contains(file.Path, "\\") ||
		file.Size <= 0 || file.Size > maxPublishedImageBytes ||
		len(file.SHA256) != sha256.Size*2 || file.SHA256 != strings.ToLower(file.SHA256) {
		return false
	}
	target := filepath.Join(workDir, filepath.FromSlash(file.Path))
	if _, ok := lexicalRelativePath(generatedDir, target); !ok {
		return false
	}
	return filepath.Base(target) == file.SHA256[:32]+".png"
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
	priorManifest, err := validateEvidenceNamespaceOwnership(sctx, location)
	if err != nil {
		return err
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
		if _, err := git.Run(ctx, sctx.WorkDir, "--literal-pathspecs", "reset", "--quiet", "HEAD", "--", targetRel); err != nil {
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
		if _, err := git.Run(ctx, sctx.WorkDir, "--literal-pathspecs", "add", "-f", "--", targetRel); err != nil {
			return fmt.Errorf("stage test evidence: %w", err)
		}
		if !matchesStagedEvidenceManifest(ctx, sctx.WorkDir, targetRel, destination.artifact) {
			if _, err := git.Run(ctx, sctx.WorkDir, "--literal-pathspecs", "reset", "--quiet", "HEAD", "--", targetRel); err != nil {
				return fmt.Errorf("clear invalid staged test evidence: %w", err)
			}
			continue
		}
		destination.published = true
		destinations[targetRel] = destination
	}

	currentManifest := generatedEvidenceManifest{Version: 1}
	for targetRel, destination := range destinations {
		if !destination.published {
			continue
		}
		currentManifest.Files = append(currentManifest.Files, generatedEvidenceManifestFile{
			Path:   targetRel,
			SHA256: destination.artifact.SHA256,
			Size:   destination.artifact.Size,
		})
	}
	sort.Slice(currentManifest.Files, func(i, j int) bool {
		return currentManifest.Files[i].Path < currentManifest.Files[j].Path
	})
	if githubRemote && (len(priorManifest.Files) > 0 || len(currentManifest.Files) > 0) {
		if err := replaceGeneratedEvidenceManifest(sctx, location, priorManifest, currentManifest); err != nil {
			return err
		}
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

func replaceGeneratedEvidenceManifest(sctx *pipeline.StepContext, location testEvidenceLocation, prior, current generatedEvidenceManifest) error {
	currentPaths := make(map[string]bool, len(current.Files))
	for _, file := range current.Files {
		currentPaths[file.Path] = true
	}
	for _, file := range prior.Files {
		if currentPaths[file.Path] {
			continue
		}
		target := filepath.Join(sctx.WorkDir, filepath.FromSlash(file.Path))
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove obsolete generated evidence: %w", err)
		}
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "--literal-pathspecs", "add", "-A", "--", file.Path); err != nil {
			return fmt.Errorf("stage obsolete generated evidence removal: %w", err)
		}
	}
	manifestRel, manifestPath, ok := generatedEvidenceManifestPath(sctx.WorkDir)
	if !ok {
		return fmt.Errorf("resolve generated evidence manifest")
	}
	if info, err := os.Lstat(manifestPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("generated evidence manifest is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect generated evidence manifest: %w", err)
	}
	if err := os.MkdirAll(location.GeneratedRepoDir, 0o755); err != nil {
		return fmt.Errorf("create generated evidence namespace: %w", err)
	}
	raw, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("encode generated evidence manifest: %w", err)
	}
	raw = append(raw, '\n')
	temp, err := os.CreateTemp(location.GeneratedRepoDir, ".manifest-*")
	if err != nil {
		return fmt.Errorf("create generated evidence manifest: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return fmt.Errorf("set generated evidence manifest mode: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("write generated evidence manifest: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close generated evidence manifest: %w", err)
	}
	if err := os.Rename(tempPath, manifestPath); err != nil {
		return fmt.Errorf("install generated evidence manifest: %w", err)
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "--literal-pathspecs", "add", "-f", "--", manifestRel); err != nil {
		return fmt.Errorf("stage generated evidence manifest: %w", err)
	}
	staged, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "show", ":"+manifestRel)
	if err != nil || !bytes.Equal(staged, raw) {
		return fmt.Errorf("verify staged generated evidence manifest")
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
	expectedPublished := make(map[string]generatedEvidenceManifestFile)
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
			repoPath, ok := reservedEvidenceDestinationPath(sctx.WorkDir, location.RepoDir, artifact)
			if !ok {
				continue
			}
			if artifact.Published {
				expectedPublished[repoPath] = generatedEvidenceManifestFile{Path: repoPath, SHA256: artifact.SHA256, Size: artifact.Size}
			}
			objectSpec := headSHA + ":" + filepath.ToSlash(repoPath)
			if matchesGitEvidenceBlob(sctx.Ctx, sctx.WorkDir, objectSpec, artifact) {
				continue
			}
			if !artifact.Published && !gitEvidenceBlobExists(sctx.Ctx, sctx.WorkDir, objectSpec) {
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
	committedManifest, manifestExists, manifestErr := loadGeneratedEvidenceManifestAtRef(sctx.Ctx, sctx.WorkDir, headSHA, location)
	if manifestErr != nil {
		invalid = true
	}
	if len(expectedPublished) > 0 {
		if !manifestExists || len(committedManifest.Files) != len(expectedPublished) {
			invalid = true
		} else {
			for _, file := range committedManifest.Files {
				if expectedPublished[file.Path] != file {
					invalid = true
					break
				}
			}
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
