package pipeline

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ValidateRecoverableDeliveryRun proves that an active run interrupted by a
// daemon crash is still at the exact checkpoint and has a single unambiguous
// push/PR/CI continuation. It performs no writes.
func ValidateRecoverableDeliveryRun(ctx context.Context, database *db.DB, p *paths.Paths, cfg *config.Config, run *db.Run, steps []Step) error {
	if run == nil || (run.Status != types.RunPending && run.Status != types.RunRunning) || run.AwaitingAgentSince != nil || run.CustodyReturnedAt != nil {
		return fmt.Errorf("run is not an active delivery checkpoint")
	}
	if run.NoMistakesVersion == nil || *run.NoMistakesVersion != buildinfo.CurrentVersion() ||
		run.NoMistakesBuildSHA == nil || *run.NoMistakesBuildSHA != buildinfo.Commit {
		return fmt.Errorf("delivery checkpoint was created by a different or unknown build")
	}
	checkpoint, err := database.GetValidationCheckpoint(run.ID)
	if err != nil {
		return err
	}
	if checkpoint == nil {
		return fmt.Errorf("delivery checkpoint is absent")
	}
	if err := ValidateValidationCheckpoint(ctx, database, p, cfg, run, checkpoint); err != nil {
		return err
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		return err
	}
	if len(results) != len(steps) || len(steps) != len(types.AllSteps()) {
		return fmt.Errorf("delivery checkpoint step count changed")
	}
	unfinished := false
	for index, step := range steps {
		result := results[index]
		if result.StepName != step.Name() || result.StepOrder != step.Name().Order() {
			return fmt.Errorf("delivery checkpoint step order changed")
		}
		if step.Name().Order() <= types.StepLint.Order() {
			if !isCompletedValidationStatus(result.Status) {
				return fmt.Errorf("delivery checkpoint validation step %s is %s", step.Name(), result.Status)
			}
			continue
		}
		if !unfinished && result.Status == types.StepStatusCompleted {
			continue
		}
		if !unfinished && (result.Status == types.StepStatusPending || result.Status == types.StepStatusRunning) {
			if step.Name() == types.StepCI && result.Status == types.StepStatusRunning {
				return fmt.Errorf("delivery checkpoint cannot restore volatile CI monitor state")
			}
			unfinished = true
			continue
		}
		if unfinished && result.Status == types.StepStatusPending {
			continue
		}
		return fmt.Errorf("delivery checkpoint history is ambiguous at %s", step.Name())
	}
	return nil
}

// PrepareValidationReuse finds only the immediately preceding same-branch run.
// It never searches past a newer run: a superseding attempt is an invalidation
// boundary even when an older checkpoint happens to match. On success it
// copies the immutable validation logs/artifacts and atomically installs cloned
// validation rows plus pending delivery rows on target.
func PrepareValidationReuse(ctx context.Context, database *db.DB, p *paths.Paths, cfg *config.Config, target *db.Run, workDir string) (string, error) {
	if database == nil || p == nil || cfg == nil || target == nil {
		return "", fmt.Errorf("prepare validation reuse: incomplete inputs")
	}
	if target.Status != types.RunPending || target.AwaitingAgentSince != nil || target.CustodyReturnedAt != nil {
		return "", fmt.Errorf("prepare validation reuse: target is not a fresh pending run")
	}
	runs, err := database.GetRunsByRepo(target.RepoID)
	if err != nil {
		return "", fmt.Errorf("prepare validation reuse: list runs: %w", err)
	}
	var source *db.Run
	for _, candidate := range runs {
		if candidate.ID == target.ID || candidate.Branch != target.Branch {
			continue
		}
		source = candidate
		break
	}
	if source == nil {
		return "", fmt.Errorf("prepare validation reuse: no preceding run")
	}
	if err := validateDeliveryReuseSource(database, target, source); err != nil {
		return "", err
	}
	checkpoint, err := database.GetValidationCheckpoint(source.ID)
	if err != nil {
		return "", fmt.Errorf("prepare validation reuse: %w", err)
	}
	if checkpoint == nil {
		return "", fmt.Errorf("prepare validation reuse: source checkpoint is absent")
	}
	if err := ValidateValidationCheckpoint(ctx, database, p, cfg, source, checkpoint); err != nil {
		return "", fmt.Errorf("prepare validation reuse: %w", err)
	}
	if checkpoint.ValidatedSHA != target.HeadSHA || checkpoint.BaseSHA != target.BaseSHA || checkpoint.IntentHash != runIntentHash(target) {
		return "", fmt.Errorf("prepare validation reuse: target commit, base, or intent changed")
	}
	if err := validateCheckpointWorktree(ctx, workDir, checkpoint.ValidatedSHA); err != nil {
		return "", fmt.Errorf("prepare validation reuse: %w", err)
	}

	sourceLogDir := p.RunLogDir(source.ID)
	targetLogDir := p.RunLogDir(target.ID)
	sourceEvidenceDir := p.RunEvidenceDir(cfg.Test.Evidence.LocalRoot, source.ID)
	targetEvidenceDir := p.RunEvidenceDir(cfg.Test.Evidence.LocalRoot, target.ID)
	cleanup := func() error {
		if err := os.RemoveAll(targetLogDir); err != nil {
			return err
		}
		return os.RemoveAll(targetEvidenceDir)
	}
	if err := cleanup(); err != nil {
		return "", fmt.Errorf("prepare validation reuse: clear target evidence: %w", err)
	}
	if err := copyValidationLogs(ctx, sourceLogDir, targetLogDir); err != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return "", fmt.Errorf("prepare validation reuse: copy logs: %v; cleanup: %w", err, cleanupErr)
		}
		return "", fmt.Errorf("prepare validation reuse: copy logs: %w", err)
	}
	if err := copyRegularTree(ctx, sourceEvidenceDir, targetEvidenceDir); err != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return "", fmt.Errorf("prepare validation reuse: copy artifacts: %v; cleanup: %w", err, cleanupErr)
		}
		return "", fmt.Errorf("prepare validation reuse: copy artifacts: %w", err)
	}
	if copied, err := hashArtifactTree(ctx, targetEvidenceDir); err != nil || !artifactHashesMatch(copied, checkpoint.EvidenceHashes) {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return "", fmt.Errorf("prepare validation reuse: copied artifacts did not verify; cleanup: %w", cleanupErr)
		}
		return "", fmt.Errorf("prepare validation reuse: copied artifact evidence did not verify")
	}
	if err := database.CloneValidatedSteps(source.ID, target.ID, targetLogDir, checkpoint); err != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return "", fmt.Errorf("prepare validation reuse: %v; cleanup: %w", err, cleanupErr)
		}
		return "", fmt.Errorf("prepare validation reuse: %w", err)
	}
	return source.ID, nil
}

func validateDeliveryReuseSource(database *db.DB, target, source *db.Run) error {
	if source.Status != types.RunFailed || source.CustodyReturnedAt != nil || source.TerminalHeadVerifiedAt == nil {
		return fmt.Errorf("prepare validation reuse: preceding run is not an eligible failed run")
	}
	if source.HeadSHA != target.HeadSHA || source.BaseSHA != target.BaseSHA {
		return fmt.Errorf("prepare validation reuse: preceding run commit or base changed")
	}
	if source.NoMistakesVersion == nil || target.NoMistakesVersion == nil || *source.NoMistakesVersion != *target.NoMistakesVersion ||
		source.NoMistakesBuildSHA == nil || target.NoMistakesBuildSHA == nil || *source.NoMistakesBuildSHA != *target.NoMistakesBuildSHA {
		return fmt.Errorf("prepare validation reuse: no-mistakes build changed or is unknown")
	}
	steps, err := database.GetStepsByRun(source.ID)
	if err != nil {
		return fmt.Errorf("prepare validation reuse: read source steps: %w", err)
	}
	if len(steps) != len(types.AllSteps()) {
		return fmt.Errorf("prepare validation reuse: source step plan is incomplete")
	}
	foundDeliveryFailure := false
	for index, name := range types.AllSteps() {
		step := steps[index]
		if step.StepName != name || step.StepOrder != name.Order() {
			return fmt.Errorf("prepare validation reuse: source step plan changed")
		}
		if name.Order() <= types.StepLint.Order() {
			if !isCompletedValidationStatus(step.Status) {
				return fmt.Errorf("prepare validation reuse: validation step %s is not complete", name)
			}
			continue
		}
		if step.Status == types.StepStatusFailed {
			if foundDeliveryFailure {
				return fmt.Errorf("prepare validation reuse: multiple failed delivery steps")
			}
			foundDeliveryFailure = true
			continue
		}
		if !foundDeliveryFailure && step.Status == types.StepStatusCompleted {
			continue
		}
		if foundDeliveryFailure && step.Status == types.StepStatusPending {
			continue
		}
		return fmt.Errorf("prepare validation reuse: delivery history is ambiguous")
	}
	if !foundDeliveryFailure {
		return fmt.Errorf("prepare validation reuse: source did not fail during delivery")
	}
	return nil
}

func copyValidationLogs(ctx context.Context, sourceDir, targetDir string) error {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}
	for _, name := range validationStepNames {
		if err := copyRegularFile(ctx, filepath.Join(sourceDir, string(name)+".log"), filepath.Join(targetDir, string(name)+".log")); err != nil {
			return err
		}
	}
	return nil
}

func copyRegularTree(ctx context.Context, sourceDir, targetDir string) error {
	info, err := os.Lstat(sourceDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source evidence root is not a directory")
	}
	return filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("evidence path escapes source root")
		}
		target := filepath.Join(targetDir, rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source evidence contains symlink")
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("source evidence contains non-regular file")
		}
		return copyRegularFile(ctx, path, target)
	})
}

func copyRegularFile(ctx context.Context, source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, &contextReader{ctx: ctx, reader: input})
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func artifactHashesMatch(actual, expected map[string]string) bool {
	for key, value := range expected {
		if key == "artifact-manifest" || strings.HasPrefix(key, "artifact:") {
			if actual[key] != value {
				return false
			}
		}
	}
	for key := range actual {
		if key == "artifact-manifest" || strings.HasPrefix(key, "artifact:") {
			if expected[key] != actual[key] {
				return false
			}
		}
	}
	return true
}
