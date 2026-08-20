package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const validationCheckpointVersion = 1

var validationStepNames = []types.StepName{
	types.StepIntent,
	types.StepRebase,
	types.StepReview,
	types.StepTest,
	types.StepDocument,
	types.StepLint,
}

type checkpointStepEvidence struct {
	Name         types.StepName
	Order        int
	Status       types.StepStatus
	ExitCode     *int
	DurationMS   *int64
	FindingsJSON *string
	Error        *string
	StartedAt    *int64
	CompletedAt  *int64
	AutoFixLimit *int
	Rounds       []checkpointRoundEvidence
}

type checkpointRoundEvidence struct {
	Round              int
	Trigger            string
	FindingsJSON       *string
	ReviewedHeadSHA    *string
	StartingHeadSHA    *string
	TrustedConfigSHA   *string
	GlobalConfigYAML   []byte
	RepoConfigYAML     []byte
	UserFindingsJSON   *string
	SelectedFindingIDs *string
	SelectionSource    *string
	FixSummary         *string
	DurationMS         int64
	CreatedAt          int64
}

// PersistValidationCheckpoint records a deterministic digest only after every
// pre-delivery step has completed with its DB round, log, and artifact evidence
// intact. A missing file or incomplete row returns an error and leaves no
// checkpoint, so later reruns take the ordinary full-validation path.
func PersistValidationCheckpoint(ctx context.Context, database *db.DB, p *paths.Paths, cfg *config.Config, run *db.Run, workDir string) (*db.ValidationCheckpoint, error) {
	if database == nil || p == nil || cfg == nil || run == nil {
		return nil, fmt.Errorf("persist validation checkpoint: incomplete inputs")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Delete first so a failed refresh can never leave older authority behind.
	if err := database.DeleteValidationCheckpoint(run.ID); err != nil {
		return nil, err
	}
	if err := validateCheckpointWorktree(ctx, workDir, run.HeadSHA); err != nil {
		return nil, err
	}
	configHash, err := pipelineConfigHash(cfg)
	if err != nil {
		return nil, err
	}
	evidenceHashes, err := collectValidationEvidence(ctx, database, p, cfg.Test.Evidence.LocalRoot, run.ID)
	if err != nil {
		return nil, err
	}
	checkpoint := &db.ValidationCheckpoint{
		RunID:          run.ID,
		Version:        validationCheckpointVersion,
		ValidatedSHA:   strings.TrimSpace(run.HeadSHA),
		BaseSHA:        strings.TrimSpace(run.BaseSHA),
		ConfigHash:     configHash,
		IntentHash:     runIntentHash(run),
		EvidenceHashes: evidenceHashes,
	}
	if err := validateCheckpointEnvelope(run, checkpoint); err != nil {
		return nil, err
	}
	if err := database.PutValidationCheckpoint(checkpoint); err != nil {
		return nil, err
	}
	return checkpoint, nil
}

// ValidateValidationCheckpoint recomputes every mechanical input. It never
// asks an agent to summarize or certify prior work.
func ValidateValidationCheckpoint(ctx context.Context, database *db.DB, p *paths.Paths, cfg *config.Config, run *db.Run, checkpoint *db.ValidationCheckpoint) error {
	if database == nil || p == nil || cfg == nil {
		return fmt.Errorf("validate validation checkpoint: incomplete inputs")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateCheckpointEnvelope(run, checkpoint); err != nil {
		return err
	}
	configHash, err := pipelineConfigHash(cfg)
	if err != nil {
		return err
	}
	if configHash != checkpoint.ConfigHash {
		return fmt.Errorf("validation checkpoint configuration changed")
	}
	if runIntentHash(run) != checkpoint.IntentHash {
		return fmt.Errorf("validation checkpoint intent changed")
	}
	actual, err := collectValidationEvidence(ctx, database, p, cfg.Test.Evidence.LocalRoot, run.ID)
	if err != nil {
		return err
	}
	if !equalHashMaps(actual, checkpoint.EvidenceHashes) {
		return fmt.Errorf("validation checkpoint evidence changed")
	}
	return nil
}

func validateCheckpointEnvelope(run *db.Run, checkpoint *db.ValidationCheckpoint) error {
	if run == nil || checkpoint == nil || checkpoint.RunID != run.ID {
		return fmt.Errorf("validation checkpoint run mismatch")
	}
	if checkpoint.Version != validationCheckpointVersion {
		return fmt.Errorf("validation checkpoint version %d is unsupported", checkpoint.Version)
	}
	if !isGitObjectID(checkpoint.ValidatedSHA) || checkpoint.ValidatedSHA != strings.TrimSpace(run.HeadSHA) {
		return fmt.Errorf("validation checkpoint commit changed or malformed")
	}
	if !isGitObjectID(checkpoint.BaseSHA) || checkpoint.BaseSHA != strings.TrimSpace(run.BaseSHA) {
		return fmt.Errorf("validation checkpoint base changed or malformed")
	}
	if !isSHA256(checkpoint.ConfigHash) || !isSHA256(checkpoint.IntentHash) {
		return fmt.Errorf("validation checkpoint metadata hash is malformed")
	}
	if len(checkpoint.EvidenceHashes) == 0 {
		return fmt.Errorf("validation checkpoint evidence is absent")
	}
	for key, value := range checkpoint.EvidenceHashes {
		if strings.TrimSpace(key) == "" || !isSHA256(value) {
			return fmt.Errorf("validation checkpoint evidence hash is malformed")
		}
	}
	return nil
}

func collectValidationEvidence(ctx context.Context, database *db.DB, p *paths.Paths, evidenceRoot, runID string) (map[string]string, error) {
	run, err := database.GetRun(runID)
	if err != nil || run == nil || run.ReviewApprovedHeadSHA == nil || !isGitObjectID(strings.TrimSpace(*run.ReviewApprovedHeadSHA)) {
		return nil, fmt.Errorf("validation review authority is absent or malformed")
	}
	results, err := database.GetStepsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("read validation steps: %w", err)
	}
	byName := make(map[types.StepName]*db.StepResult, len(results))
	for _, result := range results {
		byName[result.StepName] = result
	}
	hashes := make(map[string]string, len(validationStepNames)*2+2)
	hashes["run:review-approved-head"] = sha256Hex([]byte(strings.TrimSpace(*run.ReviewApprovedHeadSHA)))
	for _, name := range validationStepNames {
		result := byName[name]
		if result == nil || result.StepOrder != name.Order() || !isCompletedValidationStatus(result.Status) ||
			result.ExitCode == nil || result.DurationMS == nil || result.StartedAt == nil ||
			result.CompletedAt == nil || result.AgentPID != nil {
			return nil, fmt.Errorf("validation step %s evidence is incomplete", name)
		}
		rounds, err := database.GetRoundsByStep(result.ID)
		if err != nil || len(rounds) == 0 {
			return nil, fmt.Errorf("validation step %s round evidence is incomplete", name)
		}
		stepEvidence := checkpointStepEvidence{
			Name: name, Order: result.StepOrder, Status: result.Status,
			ExitCode: result.ExitCode, DurationMS: result.DurationMS,
			FindingsJSON: result.FindingsJSON, Error: result.Error,
			StartedAt: result.StartedAt, CompletedAt: result.CompletedAt,
			AutoFixLimit: result.AutoFixLimit,
			Rounds:       make([]checkpointRoundEvidence, 0, len(rounds)),
		}
		for _, round := range rounds {
			stepEvidence.Rounds = append(stepEvidence.Rounds, checkpointRoundEvidence{
				Round: round.Round, Trigger: round.Trigger, FindingsJSON: round.FindingsJSON,
				ReviewedHeadSHA: round.ReviewedHeadSHA, StartingHeadSHA: round.StartingHeadSHA,
				TrustedConfigSHA: round.TrustedConfigSHA, GlobalConfigYAML: round.GlobalConfigYAML,
				RepoConfigYAML: round.RepoConfigYAML, UserFindingsJSON: round.UserFindingsJSON,
				SelectedFindingIDs: round.SelectedFindingIDs, SelectionSource: round.SelectionSource,
				FixSummary: round.FixSummary, DurationMS: round.DurationMS, CreatedAt: round.CreatedAt,
			})
		}
		raw, err := json.Marshal(stepEvidence)
		if err != nil {
			return nil, fmt.Errorf("encode validation step %s evidence: %w", name, err)
		}
		hashes["step:"+string(name)] = sha256Hex(raw)

		expectedLog := filepath.Join(p.RunLogDir(runID), string(name)+".log")
		if result.LogPath == nil || filepath.Clean(*result.LogPath) != filepath.Clean(expectedLog) {
			return nil, fmt.Errorf("validation step %s log path is absent or divergent", name)
		}
		logHash, err := hashRegularFile(ctx, expectedLog)
		if err != nil {
			return nil, fmt.Errorf("hash validation step %s log: %w", name, err)
		}
		hashes["log:"+string(name)] = logHash
	}

	artifactHashes, err := hashArtifactTree(ctx, p.RunEvidenceDir(evidenceRoot, runID))
	if err != nil {
		return nil, err
	}
	for key, value := range artifactHashes {
		hashes[key] = value
	}
	return hashes, nil
}

func isCompletedValidationStatus(status types.StepStatus) bool {
	return status == types.StepStatusCompleted || status == types.StepStatusSkipped
}

func hashArtifactTree(ctx context.Context, root string) (map[string]string, error) {
	hashes := map[string]string{}
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		raw, _ := json.Marshal([]struct{ Path, Hash string }{})
		hashes["artifact-manifest"] = sha256Hex(raw)
		return hashes, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect validation artifact root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("validation artifact root is not a directory")
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if os.IsNotExist(walkErr) && path == root {
				return filepath.SkipDir
			}
			return walkErr
		}
		if path == root {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("validation artifact root changed type during hashing")
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("validation evidence contains symlink %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("validation evidence contains non-regular file %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("validation evidence path escapes root")
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("walk validation artifacts: %w", err)
	}
	sort.Strings(paths)
	type artifactDigest struct{ Path, Hash string }
	manifest := make([]artifactDigest, 0, len(paths))
	for _, rel := range paths {
		hash, err := hashRegularFile(ctx, filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("hash validation artifact %s: %w", rel, err)
		}
		hashes["artifact:"+rel] = hash
		manifest = append(manifest, artifactDigest{Path: rel, Hash: hash})
	}
	raw, _ := json.Marshal(manifest)
	hashes["artifact-manifest"] = sha256Hex(raw)
	return hashes, nil
}

func hashRegularFile(ctx context.Context, path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file}); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func pipelineConfigHash(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("pipeline configuration is nil")
	}
	// Exclude only local observability/retention fields that cannot affect a
	// step result. All agent, command, validation, delivery, trust, and evidence
	// publication settings remain in the digest.
	snapshot := struct {
		CaptureEvalProvenance  bool
		ReplayGlobalYAML       []byte
		ReplayRepoYAML         []byte
		TrustedConfigSHA       string
		Agent                  types.AgentName
		Agents                 []types.AgentName
		ACPXPath               string
		ACPRegistryOverrides   map[string]string
		AgentPathOverride      map[string]string
		AgentArgsOverride      map[string][]string
		CITimeout              int64
		SessionReuse           bool
		Commands               config.Commands
		IgnorePatterns         []string
		AutoFix                config.AutoFix
		CI                     config.CI
		Commit                 config.Commit
		Intent                 config.Intent
		Test                   config.Test
		Document               config.Document
		Review                 config.Review
		DisableProjectSettings bool
		NoCI                   bool
	}{
		CaptureEvalProvenance: cfg.CaptureEvalProvenance,
		ReplayGlobalYAML:      append([]byte(nil), cfg.ReplayGlobalYAML...),
		ReplayRepoYAML:        append([]byte(nil), cfg.ReplayRepoYAML...),
		TrustedConfigSHA:      cfg.TrustedConfigSHA, Agent: cfg.Agent, Agents: cfg.Agents,
		ACPXPath: cfg.ACPXPath, ACPRegistryOverrides: cfg.ACPRegistryOverrides,
		AgentPathOverride: cfg.AgentPathOverride, AgentArgsOverride: cfg.AgentArgsOverride,
		CITimeout: int64(cfg.CITimeout), SessionReuse: cfg.SessionReuse,
		Commands: cfg.Commands, IgnorePatterns: cfg.IgnorePatterns, AutoFix: cfg.AutoFix,
		CI: cfg.CI, Commit: cfg.Commit, Intent: cfg.Intent, Test: cfg.Test,
		Document: cfg.Document, Review: cfg.Review,
		DisableProjectSettings: cfg.DisableProjectSettings, NoCI: cfg.NoCI,
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("encode pipeline configuration: %w", err)
	}
	return sha256Hex(raw), nil
}

func runIntentHash(run *db.Run) string {
	if run == nil {
		return sha256Hex(nil)
	}
	source := run.IntentSource
	if source != nil && db.IsAuthoritativeRunIntentSource(*source) {
		normalized := "authoritative"
		source = &normalized
	}
	raw, _ := json.Marshal(struct {
		Intent *string
		Source *string
	}{run.Intent, source})
	return sha256Hex(raw)
}

func validateCheckpointWorktree(ctx context.Context, workDir, expectedSHA string) error {
	if strings.TrimSpace(workDir) == "" {
		return fmt.Errorf("validation checkpoint worktree is absent")
	}
	observed, err := git.HeadSHA(ctx, workDir)
	if err != nil || strings.TrimSpace(observed) != strings.TrimSpace(expectedSHA) {
		return fmt.Errorf("validation checkpoint worktree head changed or is unreadable")
	}
	status, err := git.Run(ctx, workDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil || strings.TrimSpace(status) != "" {
		return fmt.Errorf("validation checkpoint worktree is dirty or unreadable")
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func sha256Hex(raw []byte) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func equalHashMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
