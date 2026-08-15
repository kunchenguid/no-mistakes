package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

// sourceRound is a portable, JSON-stable export of a review round. It keeps
// the findings and human-decision evidence needed by the MVP but no logs,
// prompts, remote URLs, or agent session identities.
type sourceRound struct {
	ID                 string  `json:"id"`
	StepName           string  `json:"step_name,omitempty"`
	Round              int     `json:"round"`
	Trigger            string  `json:"trigger"`
	FindingsJSON       *string `json:"findings_json,omitempty"`
	ReviewedHeadSHA    *string `json:"reviewed_head_sha,omitempty"`
	UserFindingsJSON   *string `json:"user_findings_json,omitempty"`
	SelectedFindingIDs *string `json:"selected_finding_ids,omitempty"`
	SelectionSource    *string `json:"selection_source,omitempty"`
	FixSummary         *string `json:"fix_summary,omitempty"`
	DurationMS         int64   `json:"duration_ms"`
	CreatedAt          int64   `json:"created_at"`
}

func portableRound(round *db.StepRound, stepName types.StepName) sourceRound {
	return sourceRound{
		ID: round.ID, StepName: string(stepName), Round: round.Round, Trigger: round.Trigger,
		FindingsJSON: round.FindingsJSON, ReviewedHeadSHA: round.ReviewedHeadSHA,
		UserFindingsJSON: round.UserFindingsJSON, SelectedFindingIDs: round.SelectedFindingIDs,
		SelectionSource: round.SelectionSource, FixSummary: round.FixSummary,
		DurationMS: round.DurationMS, CreatedAt: round.CreatedAt,
	}
}

type sourceRun struct {
	ID                 string `json:"id"`
	Branch             string `json:"branch"`
	HeadSHA            string `json:"head_sha"`
	BaseSHA            string `json:"base_sha"`
	Status             string `json:"status"`
	Intent             string `json:"intent,omitempty"`
	IntentSource       string `json:"intent_source,omitempty"`
	NoMistakesVersion  string `json:"no_mistakes_version,omitempty"`
	NoMistakesBuildSHA string `json:"no_mistakes_build_sha,omitempty"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

type sourceStep struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	FindingsJSON string `json:"findings_json,omitempty"`
	DurationMS   *int64 `json:"duration_ms,omitempty"`
}

type sourceInvocation struct {
	StepName             string `json:"step_name"`
	Round                int    `json:"round"`
	Purpose              string `json:"purpose"`
	Agent                string `json:"agent"`
	Model                string `json:"model,omitempty"`
	DurationMS           int64  `json:"duration_ms"`
	InputTokens          int    `json:"input_tokens"`
	OutputTokens         int    `json:"output_tokens"`
	CacheReadTokens      int    `json:"cache_read_tokens"`
	FreshInputTokens     *int   `json:"fresh_input_tokens,omitempty"`
	DeltaInputTokens     *int   `json:"delta_input_tokens,omitempty"`
	DeltaOutputTokens    *int   `json:"delta_output_tokens,omitempty"`
	DeltaCacheReadTokens *int   `json:"delta_cache_read_tokens,omitempty"`
	ExitStatus           string `json:"exit_status"`
	FailureCategory      string `json:"failure_category,omitempty"`
}

// ErrNoCapturableReview marks the outcomes where a run simply holds nothing to
// freeze - no review step, no finished pass, a decision the human has not made
// yet, or rounds recorded before this machine started keeping replay
// provenance. These are ordinary states of a healthy pipeline, not failures, so
// automatic collection can pass over them silently while still surfacing a real
// capture fault. Every one of them is a deliberate refusal to invent a case:
// grading a candidate against a half-recorded review pass would be worse than
// having no case at all.
var ErrNoCapturableReview = errors.New("run has no capturable review pass")

// Capture exports every persisted review pass from one real run. It reads the
// production state only; it never starts the daemon, changes a gate, fetches,
// or sends data anywhere.
func Capture(ctx context.Context, store *Store, p *paths.Paths, database *db.DB, runID string) ([]Case, error) {
	if store == nil || p == nil || database == nil {
		return nil, fmt.Errorf("eval capture requires a store, paths, and database")
	}
	unlock, err := lockCorpus(ctx, store.root)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if err := store.cleanupPendingCaseDeletions(ctx); err != nil {
		return nil, err
	}

	run, err := database.GetRun(strings.TrimSpace(runID))
	if err != nil {
		return nil, fmt.Errorf("read source run: %w", err)
	}
	if run == nil {
		return nil, fmt.Errorf("run %q not found", runID)
	}
	repo, err := database.GetRepo(run.RepoID)
	if err != nil {
		return nil, fmt.Errorf("read source repository: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("source repository for run %q not found", runID)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		return nil, fmt.Errorf("read source steps: %w", err)
	}
	var reviewStep *db.StepResult
	allRounds := make([]sourceRound, 0)
	for _, step := range steps {
		rounds, err := database.GetRoundsByStep(step.ID)
		if err != nil {
			return nil, fmt.Errorf("read source rounds: %w", err)
		}
		for _, round := range rounds {
			allRounds = append(allRounds, portableRound(round, step.StepName))
		}
		if step.StepName == types.StepReview {
			reviewStep = step
		}
	}
	if reviewStep == nil {
		return nil, fmt.Errorf("%w: run %q has no review step", ErrNoCapturableReview, run.ID)
	}
	reviewRounds, err := database.GetRoundsByStep(reviewStep.ID)
	if err != nil {
		return nil, fmt.Errorf("read review rounds: %w", err)
	}
	if len(reviewRounds) == 0 {
		return nil, fmt.Errorf("%w: run %q recorded no review passes", ErrNoCapturableReview, run.ID)
	}

	gateDir := p.RepoDir(repo.ID)
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return nil, fmt.Errorf("source gate is unavailable for capture: %w", err)
	}
	invocations, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		return nil, fmt.Errorf("read source invocation metrics: %w", err)
	}
	captured := make([]Case, 0, len(reviewRounds))
	for i, round := range reviewRounds {
		if round.FindingsJSON == nil || strings.TrimSpace(*round.FindingsJSON) == "" {
			// An interrupted or cancelled later round is not a replayable
			// pass. Skip it so a completed sibling of the same run can still
			// be captured rather than failing the whole export.
			continue
		}
		decision := decisionForRound(round, reviewStep)
		labels := goldFromRound(round, decision, runPRState(run), reviewRounds[i+1:])
		if !labels.HasGold() && (reviewStep.Status == types.StepStatusAwaitingApproval || reviewStep.Status == types.StepStatusFixReview) {
			return nil, fmt.Errorf("%w: review round %q has no recorded gate decision", ErrNoCapturableReview, round.ID)
		}
		reviewedSHA := run.HeadSHA
		if round.ReviewedHeadSHA != nil && strings.TrimSpace(*round.ReviewedHeadSHA) != "" {
			reviewedSHA = strings.TrimSpace(*round.ReviewedHeadSHA)
		}
		if round.StartingHeadSHA == nil || strings.TrimSpace(*round.StartingHeadSHA) == "" || round.TrustedConfigSHA == nil || strings.TrimSpace(*round.TrustedConfigSHA) == "" || len(round.GlobalConfigYAML) == 0 || len(round.RepoConfigYAML) == 0 {
			// Provenance is a point-in-time snapshot of configuration that no
			// longer exists anywhere, so this can never be backfilled - only
			// later runs are capturable. Name the setting rather than the age:
			// a round recorded seconds ago with capture_provenance off fails
			// here too, and calling that "old" sends the reader hunting for a
			// version problem that is not there.
			return nil, fmt.Errorf("%w: review round %q was recorded without eval configuration provenance (eval.capture_provenance was off, or the run predates the setting); only runs reviewed after it is enabled can be captured", ErrNoCapturableReview, round.ID)
		}
		startingSHA := strings.TrimSpace(*round.StartingHeadSHA)
		trustedSHA := strings.TrimSpace(*round.TrustedConfigSHA)
		globalConfig, err := agentNeutralGlobalConfig(round.GlobalConfigYAML)
		if err != nil {
			return nil, fmt.Errorf("read review round %q global configuration: %w", round.ID, err)
		}
		if _, err := config.LoadRepoFromBytes(round.RepoConfigYAML); err != nil {
			return nil, fmt.Errorf("read review round %q repository configuration: %w", round.ID, err)
		}
		repoConfigBytes := append([]byte(nil), round.RepoConfigYAML...)
		replayBaseSHA, err := effectiveReplayBase(ctx, gateDir, run.BaseSHA, reviewedSHA, trustedSHA)
		if err != nil {
			return nil, err
		}
		if _, err := git.ResolveRef(ctx, gateDir, reviewedSHA); err != nil {
			return nil, fmt.Errorf("review round %q commit is unavailable for capture: %w", round.ID, err)
		}
		changedFiles, changedLines, err := git.DiffStat(ctx, gateDir, replayBaseSHA, reviewedSHA)
		if err != nil {
			return nil, fmt.Errorf("read review diff stat for round %q: %w", round.ID, err)
		}
		changedPaths, err := git.DiffNameOnly(ctx, gateDir, replayBaseSHA, reviewedSHA)
		if err != nil {
			return nil, fmt.Errorf("read review changed files for round %q: %w", round.ID, err)
		}
		caseID := run.ID + "-" + round.ID
		caseDir := store.caseDir(caseID)
		if existing, err := os.Stat(caseDir); err == nil && existing.IsDir() {
			c, err := relabelExistingCase(store, caseDir, round, decision, runPRState(run), reviewRounds[i+1:])
			if err != nil {
				return nil, fmt.Errorf("relabel existing case %q: %w", caseID, err)
			}
			captured = append(captured, c)
			continue
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect case destination: %w", err)
		}

		manifest := Manifest{
			Version: manifestVersion, ID: caseID, SourceRunID: run.ID, SourceRoundID: round.ID,
			CapturedAt: time.Now().Unix(), RepoFingerprint: fingerprint(repo.UpstreamURL),
			Branch: run.Branch, DefaultBranch: repo.DefaultBranch, BaseSHA: replayBaseSHA,
			HeadSHA: run.HeadSHA, StartingHeadSHA: startingSHA, ReviewedHeadSHA: reviewedSHA, TrustedConfigSHA: trustedSHA,
			ChangedFiles: changedFiles, ChangedLines: changedLines,
		}
		if run.Intent != nil {
			manifest.Intent = *run.Intent
		}
		if run.IntentSource != nil {
			manifest.IntentSource = *run.IntentSource
		}
		if run.NoMistakesVersion != nil {
			manifest.VersionAtCapture = *run.NoMistakesVersion
		}
		if run.NoMistakesBuildSHA != nil {
			manifest.BuildSHA = *run.NoMistakesBuildSHA
		}
		baseline := baselineForRound(invocations, round.Round)
		c := Case{Manifest: manifest, Labels: labels, Decision: decision, Baseline: baseline, Dir: caseDir}
		if err := writeCase(ctx, store, gateDir, c, globalConfig, repoConfigBytes, sourceRunFor(run), sourceStepsFor(steps), allRounds, sourceInvocationsFor(invocations), portableRound(round, types.StepReview), changedPaths); err != nil {
			return nil, err
		}
		c, err = loadCase(caseDir)
		if err != nil {
			return nil, fmt.Errorf("read captured case: %w", err)
		}
		if err := store.registerCase(c); err != nil {
			return nil, err
		}
		captured = append(captured, c)
	}
	if len(captured) == 0 {
		return nil, fmt.Errorf("%w: run %q has no capturable review pass", ErrNoCapturableReview, run.ID)
	}
	return captured, nil
}

// effectiveReplayBase reproduces ReviewStep's branch-scoped base: the merge
// base of the reviewed head and the pinned default branch. Run.BaseSHA is the
// received-push old SHA, which may be a previous feature tip rather than the
// review base. It is only a legacy fallback when the merge-base cannot be
// recovered from an older gate.
func effectiveReplayBase(ctx context.Context, gateDir, recordedBase, head, trustedSHA string) (string, error) {
	if base, err := git.Run(ctx, gateDir, "merge-base", head, trustedSHA); err == nil && strings.TrimSpace(base) != "" {
		return strings.TrimSpace(base), nil
	}
	if recordedBase != "" && !git.IsZeroSHA(recordedBase) {
		if _, err := git.ResolveRef(ctx, gateDir, recordedBase); err == nil {
			return recordedBase, nil
		}
	}
	return "", fmt.Errorf("derive replay base: reviewed head and trusted default branch have no readable merge base")
}

func repoConfigAt(ctx context.Context, gateDir, sha string) (*config.RepoConfig, error) {
	if _, err := git.ResolveRef(ctx, gateDir, sha); err != nil {
		return nil, err
	}
	entry, err := git.Run(ctx, gateDir, "ls-tree", sha, "--", ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("inspect repository config: %w", err)
	}
	if strings.TrimSpace(entry) == "" {
		return &config.RepoConfig{}, nil
	}
	content, err := git.ShowFile(ctx, gateDir, sha, ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("read repository config: %w", err)
	}
	return config.LoadRepoFromBytes([]byte(content))
}

func agentNeutralGlobalConfig(data []byte) ([]byte, error) {
	if _, err := config.LoadGlobalFromBytes(data); err != nil {
		return nil, fmt.Errorf("read pinned global config for capture: %w", err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse global config bytes: %w", err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	// The candidate selects agent and model explicitly. Do not accidentally
	// inherit a captured default model or agent list into a comparison.
	delete(raw, "agent")
	delete(raw, "agent_args_override")
	out, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("serialize agent-neutral global config: %w", err)
	}
	return out, nil
}

func writeCase(ctx context.Context, store *Store, gateDir string, c Case, globalConfig, repoConfig []byte, run sourceRun, steps []sourceStep, rounds []sourceRound, invocations []sourceInvocation, selectedRound sourceRound, changedFiles []string) (err error) {
	tmp, err := os.MkdirTemp(store.cases, ".capture-")
	if err != nil {
		return fmt.Errorf("create temporary case: %w", err)
	}
	defer os.RemoveAll(tmp)
	for _, dir := range []string{filepath.Join(tmp, "config"), filepath.Join(tmp, "original"), filepath.Join(tmp, "evals")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := store.storeCaseObjects(ctx, gateDir, c.RepoFingerprint, c.ID, map[string]string{
		refHead:          c.ReviewedHeadSHA,
		refSourceHead:    c.HeadSHA,
		refBase:          c.BaseSHA,
		refTrustedConfig: c.TrustedConfigSHA,
	}); err != nil {
		return err
	}
	// The case directory is published last, so a failure after this point
	// would leave pool refs pinning objects no case will ever claim. Release
	// them rather than growing an unreachable, unreclaimable remainder.
	defer func() {
		if err != nil {
			_ = dropCaseObjects(ctx, store.poolDir(c.RepoFingerprint), c.ID)
		}
	}()
	if err := os.WriteFile(filepath.Join(tmp, "config", "global.yaml"), globalConfig, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "config", "repo-config.yaml"), repoConfig, 0o644); err != nil {
		return err
	}
	for _, item := range []struct {
		path  string
		value any
	}{
		{"manifest.json", c.Manifest},
		{"labels.json", c.Labels},
		{filepath.Join("original", "run.json"), run},
		{filepath.Join("original", "steps.json"), steps},
		{filepath.Join("original", "rounds.json"), rounds},
		{filepath.Join("original", "round.json"), selectedRound},
		{filepath.Join("original", "decision.json"), c.Decision},
		{filepath.Join("original", "baseline.json"), c.Baseline},
		{filepath.Join("original", "changed-files.json"), changedFiles},
		{filepath.Join("original", "invocations.json"), invocations},
	} {
		if err := writeJSON(filepath.Join(tmp, item.path), item.value); err != nil {
			return fmt.Errorf("write case %s: %w", item.path, err)
		}
	}
	if err := os.Rename(tmp, c.Dir); err != nil {
		return fmt.Errorf("publish captured case: %w", err)
	}
	return nil
}

func sourceRunFor(run *db.Run) sourceRun {
	out := sourceRun{ID: run.ID, Branch: run.Branch, HeadSHA: run.HeadSHA, BaseSHA: run.BaseSHA, Status: string(run.Status), CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt}
	if run.Intent != nil {
		out.Intent = *run.Intent
	}
	if run.IntentSource != nil {
		out.IntentSource = *run.IntentSource
	}
	if run.NoMistakesVersion != nil {
		out.NoMistakesVersion = *run.NoMistakesVersion
	}
	if run.NoMistakesBuildSHA != nil {
		out.NoMistakesBuildSHA = *run.NoMistakesBuildSHA
	}
	return out
}

func sourceStepsFor(steps []*db.StepResult) []sourceStep {
	out := make([]sourceStep, 0, len(steps))
	for _, step := range steps {
		item := sourceStep{ID: step.ID, Name: string(step.StepName), Status: string(step.Status), DurationMS: step.DurationMS}
		if step.FindingsJSON != nil {
			item.FindingsJSON = *step.FindingsJSON
		}
		out = append(out, item)
	}
	return out
}

func sourceInvocationsFor(invocations []db.AgentInvocation) []sourceInvocation {
	out := make([]sourceInvocation, 0, len(invocations))
	for _, inv := range invocations {
		out = append(out, sourceInvocation{StepName: inv.StepName, Round: inv.Round, Purpose: inv.Purpose, Agent: inv.Agent, Model: inv.Model, DurationMS: inv.DurationMS, InputTokens: inv.InputTokens, OutputTokens: inv.OutputTokens, CacheReadTokens: inv.CacheReadTokens, FreshInputTokens: inv.FreshInputTokens, DeltaInputTokens: inv.DeltaInputTokens, DeltaOutputTokens: inv.DeltaOutputTokens, DeltaCacheReadTokens: inv.DeltaCacheReadTokens, ExitStatus: inv.ExitStatus, FailureCategory: inv.FailureCategory})
	}
	return out
}

func baselineForRound(invocations []db.AgentInvocation, round int) BaselineMetrics {
	var baseline BaselineMetrics
	seen := false
	complete := true
	for _, inv := range invocations {
		if inv.StepName != string(types.StepReview) || inv.Round != round || inv.Purpose != "review" {
			continue
		}
		seen = true
		baseline.DurationMS += inv.DurationMS
		if inv.DeltaInputTokens == nil || inv.DeltaOutputTokens == nil || inv.DeltaCacheReadTokens == nil {
			complete = false
			continue
		}
		baseline.InputTokens += int64(*inv.DeltaInputTokens)
		baseline.OutputTokens += int64(*inv.DeltaOutputTokens)
		baseline.CacheReadTokens += int64(*inv.DeltaCacheReadTokens)
		baseline.FreshInputTokens += int64(agent.FreshInputTokens(*inv.DeltaInputTokens, *inv.DeltaCacheReadTokens))
	}
	baseline.TokensReported = seen && complete
	if !baseline.TokensReported {
		baseline.InputTokens = 0
		baseline.OutputTokens = 0
		baseline.CacheReadTokens = 0
		baseline.FreshInputTokens = 0
	}
	return baseline
}

func decisionForRound(round *db.StepRound, step *db.StepResult) Decision {
	decision := Decision{Action: "unknown"}
	if round.SelectionSource != nil {
		decision.SelectionSource = *round.SelectionSource
	}
	if round.SelectedFindingIDs != nil {
		_ = json.Unmarshal([]byte(*round.SelectedFindingIDs), &decision.SelectedFindingIDs)
	}
	if round.UserFindingsJSON != nil && strings.TrimSpace(*round.UserFindingsJSON) != "" {
		decision.HasUserFindings = true
	}
	if len(decision.SelectedFindingIDs) > 0 {
		decision.Action = "fix"
		return decision
	}
	if step == nil {
		return decision
	}
	if step.Status == types.StepStatusSkipped {
		decision.Action = "skip"
		return decision
	}
	if step.Status == types.StepStatusFailed && step.Error != nil && strings.Contains(*step.Error, "aborted by user") {
		decision.Action = "abort"
		return decision
	}
	if round.FindingsJSON != nil {
		findings, err := types.ParseFindingsJSON(*round.FindingsJSON)
		if err == nil && types.HasAskUserFindings(findings) && step.Status == types.StepStatusCompleted {
			decision.Action = "approve"
		}
	}
	return decision
}

// goldFromRound writes labels the recorded gate evidence supports.
// A user-selected finding is true-positive gold without merge. An auto-fix
// selection is true-positive gold only when the source PR merged and the fix
// landed. A raised auto-fix/ask-user finding that shipped unfixed in a merged
// PR is false-positive gold. An intermediate re-raise that is gone from the
// last later review round is treated as fixed, not shipped-unfixed. Skip,
// approve-with-findings on an unmerged PR, reverted auto-fixes, and no-op
// notes stay unlabeled. Confirmed post-PR misses are written later by
// IngestPostPRMiss, not here.
func goldFromRound(round *db.StepRound, decision Decision, prState string, later []*db.StepRound) Labels {
	labels := Labels{Version: labelsVersion}
	byID := findingIndex(round)
	seen := map[string]bool{}
	selected := map[string]bool{}
	for _, id := range decision.SelectedFindingIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			selected[id] = true
		}
	}
	if decision.SelectionSource == db.RoundSelectionSourceUser {
		for _, id := range decision.SelectedFindingIDs {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			finding, ok := byID[id]
			if !ok {
				continue
			}
			if finding.Source == types.FindingSourceUser {
				continue
			}
			seen[id] = true
			labels.Findings = append(labels.Findings, goldForRecordedFinding(finding, goldSourceUserFix, GoldTruePositive))
		}
	}
	if decision.SelectionSource == db.RoundSelectionSourceAutoFix && prState == "merged" {
		for _, id := range decision.SelectedFindingIDs {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			finding, ok := byID[id]
			if !ok {
				continue
			}
			if !fixLanded(finding, later) {
				continue
			}
			seen[id] = true
			labels.Findings = append(labels.Findings, goldForRecordedFinding(finding, goldSourceAutoFixMerged, GoldTruePositive))
		}
	}
	if round.UserFindingsJSON != nil && strings.TrimSpace(*round.UserFindingsJSON) != "" {
		for _, finding := range parseFindingItems(*round.UserFindingsJSON) {
			if finding.Source != types.FindingSourceUser {
				continue
			}
			id := strings.TrimSpace(finding.ID)
			if id != "" && seen[id] {
				continue
			}
			if id != "" {
				seen[id] = true
			}
			labels.Findings = append(labels.Findings, goldForRecordedFinding(finding, goldSourceUserAdded, GoldFalseNegative))
		}
	}
	if prState == "merged" && round.FindingsJSON != nil {
		for _, finding := range parseFindingItems(*round.FindingsJSON) {
			id := strings.TrimSpace(finding.ID)
			if id == "" || seen[id] || selected[id] {
				continue
			}
			if finding.ActionOrDefault() == types.ActionNoOp {
				continue
			}
			if !stillRaisedAtMerge(finding, later) {
				continue
			}
			seen[id] = true
			labels.Findings = append(labels.Findings, goldForRecordedFinding(finding, goldSourceShippedUnfixed, GoldFalsePositive))
		}
	}
	return labels
}

func goldForRecordedFinding(finding types.Finding, source, kind string) FindingGold {
	return FindingGold{
		ID:          finding.ID,
		Kind:        kind,
		Source:      source,
		File:        finding.File,
		Line:        finding.Line,
		Description: finding.Description,
		Severity:    finding.Severity,
		Action:      finding.Action,
	}
}

func fixLanded(finding types.Finding, later []*db.StepRound) bool {
	gold := FindingGold{ID: finding.ID, File: finding.File, Description: finding.Description}
	for _, round := range later {
		if round == nil || round.FindingsJSON == nil {
			continue
		}
		for _, item := range parseFindingItems(*round.FindingsJSON) {
			if sameUnderlyingIssue(gold, item) {
				return false
			}
		}
	}
	return true
}

// stillRaisedAtMerge reports whether the finding is still present in the last
// later review round, which is the merge-time evidence for shipped-unfixed FP.
// An intermediate re-raise that disappears before that last round is a fix,
// not a shipped-unfixed finding. No later round means this round is last.
func stillRaisedAtMerge(finding types.Finding, later []*db.StepRound) bool {
	last := lastLaterRound(later)
	if last == nil {
		return true
	}
	if last.FindingsJSON == nil {
		return false
	}
	gold := FindingGold{ID: finding.ID, File: finding.File, Description: finding.Description}
	for _, item := range parseFindingItems(*last.FindingsJSON) {
		if sameUnderlyingIssue(gold, item) {
			return true
		}
	}
	return false
}

func lastLaterRound(later []*db.StepRound) *db.StepRound {
	var last *db.StepRound
	for _, round := range later {
		if round == nil {
			continue
		}
		if last == nil || round.Round > last.Round {
			last = round
		}
	}
	return last
}

func runPRState(run *db.Run) string {
	if run == nil || run.PRState == nil || strings.TrimSpace(*run.PRState) == "" {
		return "none"
	}
	return strings.ToLower(strings.TrimSpace(*run.PRState))
}

func mergeGold(existing, computed Labels) Labels {
	out := Labels{
		Version:                 labelsVersion,
		QueuedCandidateFindings: existing.QueuedCandidateFindings,
	}
	keptID := map[string]bool{}
	for _, g := range existing.Findings {
		if isDerivedMergeGold(g.Source) {
			continue
		}
		out.Findings = append(out.Findings, g)
		if id := strings.TrimSpace(g.ID); id != "" {
			keptID[id] = true
		}
	}
	for _, g := range computed.Findings {
		id := strings.TrimSpace(g.ID)
		if id != "" && keptID[id] {
			continue
		}
		out.Findings = append(out.Findings, g)
		if id != "" {
			keptID[id] = true
		}
	}
	return out
}

func isDerivedMergeGold(source string) bool {
	return source == goldSourceAutoFixMerged || source == goldSourceShippedUnfixed
}

// RelabelRun recomputes gold for every captured case of a run. It adds new
// auto-fix-merged / shipped-unfixed labels onto previously unlabeled findings
// and drops obsolete derived merge labels that the current rounds no longer
// support. It never overwrites adjudicated, user-fix, or ingested post-PR-miss
// labels. Missing cases are a no-op so a merge observed before the first
// capture cannot fail the pipeline.
func RelabelRun(ctx context.Context, store *Store, p *paths.Paths, database *db.DB, runID string) ([]Case, error) {
	if store == nil || p == nil || database == nil {
		return nil, fmt.Errorf("eval relabel requires a store, paths, and database")
	}
	unlock, err := lockCorpus(ctx, store.root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return relabelRunLocked(store, database, runID)
}

func RelabelAll(ctx context.Context, store *Store, p *paths.Paths, database *db.DB) ([]Case, error) {
	if store == nil || p == nil || database == nil {
		return nil, fmt.Errorf("eval relabel requires a store, paths, and database")
	}
	cases, err := store.ListCases("all")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Case
	for _, c := range cases {
		if seen[c.SourceRunID] {
			continue
		}
		seen[c.SourceRunID] = true
		relabeled, err := RelabelRun(ctx, store, p, database, c.SourceRunID)
		if err != nil {
			return out, err
		}
		out = append(out, relabeled...)
	}
	return out, nil
}

func relabelRunLocked(store *Store, database *db.DB, runID string) ([]Case, error) {
	run, reviewRounds, reviewStep, err := loadReviewRounds(database, runID)
	if err != nil {
		return nil, err
	}
	if run == nil || len(reviewRounds) == 0 {
		return nil, nil
	}
	existing, err := store.casesForRun(run.ID)
	if err != nil {
		return nil, err
	}
	byRound := map[string]*db.StepRound{}
	for _, round := range reviewRounds {
		byRound[round.ID] = round
	}
	out := make([]Case, 0, len(existing))
	for _, c := range existing {
		round := byRound[c.SourceRoundID]
		if round == nil {
			out = append(out, c)
			continue
		}
		later := laterReviewRounds(reviewRounds, round)
		decision := decisionForRound(round, reviewStep)
		updated, err := relabelExistingCase(store, c.Dir, round, decision, runPRState(run), later)
		if err != nil {
			return nil, err
		}
		out = append(out, updated)
	}
	return out, nil
}

func loadReviewRounds(database *db.DB, runID string) (*db.Run, []*db.StepRound, *db.StepResult, error) {
	run, err := database.GetRun(strings.TrimSpace(runID))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read source run: %w", err)
	}
	if run == nil {
		return nil, nil, nil, nil
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read source steps: %w", err)
	}
	var reviewStep *db.StepResult
	for _, step := range steps {
		if step.StepName == types.StepReview {
			reviewStep = step
			break
		}
	}
	if reviewStep == nil {
		return run, nil, nil, nil
	}
	rounds, err := database.GetRoundsByStep(reviewStep.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read review rounds: %w", err)
	}
	return run, rounds, reviewStep, nil
}

func laterReviewRounds(rounds []*db.StepRound, current *db.StepRound) []*db.StepRound {
	if current == nil {
		return nil
	}
	out := make([]*db.StepRound, 0)
	for _, round := range rounds {
		if round.Round > current.Round {
			out = append(out, round)
		}
	}
	return out
}

func relabelExistingCase(store *Store, dir string, round *db.StepRound, decision Decision, prState string, later []*db.StepRound) (Case, error) {
	c, err := loadCase(dir)
	if err != nil {
		return Case{}, err
	}
	computed := goldFromRound(round, decision, prState, later)
	c.Labels = mergeGold(c.Labels, computed)
	if err := writeJSON(filepath.Join(c.Dir, "labels.json"), c.Labels); err != nil {
		return Case{}, fmt.Errorf("write relabeled gold: %w", err)
	}
	if err := store.registerCase(c); err != nil {
		return Case{}, err
	}
	if err := store.updateCaseGoldCount(c.ID, len(c.Labels.Findings)); err != nil {
		return Case{}, err
	}
	return c, nil
}

func findingIndex(round *db.StepRound) map[string]types.Finding {
	index := map[string]types.Finding{}
	if round == nil {
		return index
	}
	if round.FindingsJSON != nil {
		for _, finding := range parseFindingItems(*round.FindingsJSON) {
			if id := strings.TrimSpace(finding.ID); id != "" {
				index[id] = finding
			}
		}
	}
	if round.UserFindingsJSON != nil {
		for _, finding := range parseFindingItems(*round.UserFindingsJSON) {
			if id := strings.TrimSpace(finding.ID); id != "" {
				index[id] = finding
			}
		}
	}
	return index
}

func fingerprint(rawURL string) string {
	value := safeurl.Redact(rawURL)
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func highestSeverity(raw *string) string {
	if raw == nil {
		return "none"
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return "unknown"
	}
	rank := map[string]int{"none": 0, "info": 1, "warning": 2, "error": 3}
	highest := "none"
	for _, finding := range findings.Items {
		if rank[finding.Severity] > rank[highest] {
			highest = finding.Severity
		}
	}
	return highest
}
