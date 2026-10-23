package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

// retryLaunch is what a continuation run takes from the failed run it
// continues: the run itself, whose record the executor re-proves and carries,
// and the audit link written before the continuation can execute anything.
type retryLaunch struct {
	source     *db.Run
	link       db.RunRetry
	gates      []config.Gate
	checkpoint *pipeline.RetryCheckpoint
}

// record writes the audit link and the failed run's exact intent onto the
// continuation run. The intent is copied byte for byte with its provenance
// because a carried Intent step never runs again to produce it.
func (r *retryLaunch) record(database *db.DB, run *db.Run) error {
	link := r.link
	link.RunID = run.ID
	if err := database.InsertRunRetry(link); err != nil {
		return err
	}
	if r.source.Intent == nil {
		return nil
	}
	intent := db.RunIntent{Summary: *r.source.Intent}
	if r.source.IntentSource != nil {
		intent.Source = *r.source.IntentSource
	}
	if r.source.IntentSessionID != nil {
		intent.SessionID = *r.source.IntentSessionID
	}
	if r.source.IntentScore != nil {
		intent.Score = *r.source.IntentScore
	}
	if err := database.UpdateRunIntent(run.ID, intent); err != nil {
		return fmt.Errorf("copy the failed run's intent: %w", err)
	}
	run.Intent, run.IntentSource, run.IntentSessionID, run.IntentScore = &intent.Summary, &intent.Source, &intent.SessionID, &intent.Score
	return nil
}

// retryRefusal names the proof a retry request failed. Every refusal leaves
// the failed run, the branch, and the gate untouched.
type retryRefusal struct {
	runID  string
	proof  string
	detail string
}

func (r *retryRefusal) Error() string {
	return fmt.Sprintf("refusing retry of run %s: %s proof failed: %s; `no-mistakes rerun` validates the branch from the start instead", r.runID, r.proof, r.detail)
}

func refuseRetry(runID, proof, format string, args ...any) error {
	return &retryRefusal{runID: runID, proof: proof, detail: fmt.Sprintf(format, args...)}
}

// HandleRetry continues runID, the branch's latest run, in a new run that
// starts at the failed run's preserved head: the steps before its failed step
// are carried, the failed step restarts from the beginning, and every later
// step runs normally. It refuses, changing nothing, unless every proof holds:
// the run is this branch's latest and failed while nothing is active; its
// record names exactly one failed step, failed on an agent invocation error,
// with nothing from Test on carried and nothing published; its preserved head
// is anchored; the caller is clean at exactly that head; the model profile and
// the repository's gates are unchanged; and the repository configuration, the
// recorded global configuration, and the agent and model that served each
// carried step are the ones a new run would use.
func (m *RunManager) HandleRetry(ctx context.Context, repoID, branch, runID, callerHeadSHA string) (*db.RunRetry, error) {
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("unknown repo %s", repoID)
	}
	var link db.RunRetry
	_, err = m.withBranchLock(repo.ID, branch, func() (string, error) {
		launch, err := m.proveRetry(ctx, repo, branch, runID, callerHeadSHA)
		if err != nil {
			return "", err
		}
		if m.afterRetryProof != nil {
			m.afterRetryProof()
		}
		source := launch.source
		prBaseBranch := ""
		if source.PRBaseBranch != nil {
			prBaseBranch = strings.TrimSpace(*source.PRBaseBranch)
		}
		var profiles []*agentcfg.PiProfile
		if source.PiProfile != nil {
			profiles = append(profiles, source.PiProfile)
		}
		continuation, err := m.startRunWithIntentSourceLocked(ctx, repo, branch, source.HeadSHA, source.BaseSHA, "retry", nil, "", "", "", "", "", prBaseBranch, source.OmitIntent, inheritablePRURL(source), "", source.ClosingIssueRefs, launch, profiles...)
		link = launch.link
		link.RunID = continuation
		return continuation, err
	})
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func (m *RunManager) proveRetry(ctx context.Context, repo *db.Repo, branch, runID, callerHeadSHA string) (*retryLaunch, error) {
	source, err := m.db.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	if source == nil || source.RepoID != repo.ID || source.Branch != branch {
		return nil, refuseRetry(runID, "run identity", "the run does not belong to branch %s of this repository", branch)
	}
	runs, err := m.db.GetRunsByRepo(repo.ID)
	if err != nil {
		return nil, fmt.Errorf("get runs: %w", err)
	}
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if run.ID != source.ID {
			return nil, refuseRetry(runID, "latest run", "run %s is newer on branch %s, so this run's head may no longer be the branch's", run.ID, branch)
		}
		break
	}
	active, err := m.db.GetActiveRun(repo.ID, branch)
	if err != nil {
		return nil, fmt.Errorf("get active run: %w", err)
	}
	if active != nil {
		return nil, refuseRetry(runID, "no active run", "run %s is active on branch %s", active.ID, branch)
	}
	if source.Status != types.RunFailed {
		return nil, refuseRetry(runID, "failed run", "the run is %s, and only a failed run can continue", source.Status)
	}
	if source.LastPushedSHA != nil || source.PushGeneration != nil {
		return nil, refuseRetry(runID, "unpublished run", "the run already pushed, so its head is no longer only in the local gate")
	}
	if source.VerificationPlan != nil {
		return nil, refuseRetry(runID, "compatible configuration", "the run carries a verification plan, which belongs to that run alone")
	}
	gates, err := m.pinnedRunGates(source.ID)
	if err != nil {
		return nil, refuseRetry(runID, pipeline.RetryProofCheckpoint, "%v", err)
	}
	checkpoint, err := pipeline.PlanRetryCheckpoint(m.db, source, steps.WithCustomGates(m.steps(), gates))
	if err != nil {
		var refusal *pipeline.RetryRefusal
		if errors.As(err, &refusal) {
			return nil, refuseRetry(runID, refusal.Proof, "%s", refusal.Reason)
		}
		return nil, err
	}
	if err := m.provePreservedHead(ctx, repo, source); err != nil {
		return nil, refuseRetry(runID, "preserved head", "%v", err)
	}
	switch callerHeadSHA {
	case "":
		return nil, refuseRetry(runID, "clean branch", "the working tree has uncommitted changes, so its head is not a proof")
	case source.HeadSHA:
	default:
		return nil, refuseRetry(runID, "matching head", "the clean local head %s is not the run's preserved head %s; inspect `no-mistakes axi status` and follow `branch_sync.next_action.command` (`no-mistakes axi sync --recover` adopts the preserved head) before retrying", callerHeadSHA, source.HeadSHA)
	}
	if err := m.proveRetryConfiguration(ctx, repo, source, gates, checkpoint); err != nil {
		return nil, refuseRetry(runID, "compatible configuration", "%v", err)
	}
	failed := checkpoint.Failed()
	sourceError := ""
	if failed.Error != nil {
		sourceError = *failed.Error
	}
	return &retryLaunch{source: source, gates: gates, checkpoint: checkpoint, link: db.RunRetry{
		SourceRunID:       source.ID,
		StepName:          failed.StepName,
		SourceRound:       checkpoint.Round,
		SourceError:       sourceError,
		CheckpointHeadSHA: source.HeadSHA,
	}}, nil
}

// provePreservedHead requires the head the failed run verified when it ended
// to still be the exact commit the gate keeps for it: its recovery anchor when
// the pipeline moved the head, the submitted commit otherwise.
func (m *RunManager) provePreservedHead(ctx context.Context, repo *db.Repo, source *db.Run) error {
	if source.TerminalHeadVerifiedAt == nil {
		return fmt.Errorf("the run's final head was not verified when it failed")
	}
	gateDir := m.paths.RepoDir(repo.ID)
	if source.SubmittedHeadSHA != nil && *source.SubmittedHeadSHA == source.HeadSHA {
		if preserved, err := git.Run(ctx, gateDir, "rev-parse", source.HeadSHA+"^{commit}"); err != nil || preserved != source.HeadSHA {
			return fmt.Errorf("head %s is not available in the local gate", source.HeadSHA)
		}
		return nil
	}
	ref := custody.RecoveryRef(source.ID)
	_, exists, err := git.ExactRefTarget(ctx, gateDir, ref)
	if err != nil {
		return fmt.Errorf("inspect recovery anchor %s: %w", ref, err)
	}
	if !exists {
		return fmt.Errorf("recovery anchor %s is missing", ref)
	}
	preserved, err := git.Run(ctx, gateDir, "rev-parse", ref+"^{commit}")
	if err != nil {
		return fmt.Errorf("recovery anchor %s does not name a commit", ref)
	}
	if preserved != source.HeadSHA {
		return fmt.Errorf("recovery anchor %s points at %s, not the run's head %s", ref, preserved, source.HeadSHA)
	}
	return nil
}

// proveRetryConfiguration requires what the failed run pinned at creation to
// resolve the same way now: the model profile, and the gates, which decide the
// step sequence the carried record is matched against. It also requires the
// configuration that produced every carried step result to be the one a new
// run would use: the repository configuration the carried Review rounds
// recorded, and the agent that served the carried steps.
func (m *RunManager) proveRetryConfiguration(ctx context.Context, repo *db.Repo, source *db.Run, pinned []config.Gate, checkpoint *pipeline.RetryCheckpoint) error {
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return fmt.Errorf("load global config: %w", err)
	}
	if source.PiProfile != nil {
		resolved, err := globalCfg.ResolvePiProfile(source.PiProfile)
		if err != nil {
			return fmt.Errorf("the run's model profile no longer resolves: %w", err)
		}
		if *resolved != *source.PiProfile {
			return fmt.Errorf("the run's model profile now resolves to %s/%s instead of %s/%s", resolved.Model, resolved.Effort, source.PiProfile.Model, source.PiProfile.Effort)
		}
	}
	cfg, currentRepoCfg, err := m.effectiveConfigBeforeRun(ctx, repo, source.HeadSHA, globalCfg, "retry configuration")
	if err != nil {
		return err
	}
	return m.proveConfigurationMatches(ctx, source, globalCfg, cfg, currentRepoCfg, pinned, checkpoint)
}

// proveConfigurationMatches compares a resolved configuration with the one the
// failed run and its carried steps ran under. The launch calls it again with
// the configuration the continuation run actually loaded, so a change between
// the proof and the launch cannot carry a Review approval.
func (m *RunManager) proveConfigurationMatches(ctx context.Context, source *db.Run, globalCfg *config.GlobalConfig, cfg *config.Config, currentRepoCfg *config.RepoConfig, pinned []config.Gate, checkpoint *pipeline.RetryCheckpoint) error {
	current, err := config.MarshalGates(cfg.Gates)
	if err != nil {
		return fmt.Errorf("resolve the repository's gates: %w", err)
	}
	recorded, err := config.MarshalGates(pinned)
	if err != nil {
		return fmt.Errorf("read the run's gates: %w", err)
	}
	if current != recorded {
		return fmt.Errorf("the repository's gates changed since the run started, so its step sequence would differ")
	}
	carried := checkpoint.Steps[:checkpoint.Index]
	if err := m.proveCarriedReviewConfig(globalCfg, currentRepoCfg, carried); err != nil {
		return err
	}
	return m.proveCarriedAgent(ctx, source, cfg, carried)
}

// proveCarriedReviewConfig compares the configuration a carried Review ran
// under with the configuration a new run would get now: the repository
// effective repository configuration and the global configuration its rounds
// recorded when they ran.
func (m *RunManager) proveCarriedReviewConfig(globalCfg *config.GlobalConfig, current *config.RepoConfig, carried []*db.StepResult) error {
	currentYAML, err := yaml.Marshal(current)
	if err != nil {
		return fmt.Errorf("serialize the repository configuration: %w", err)
	}
	for _, row := range carried {
		if row.StepName != types.StepReview || row.Status != types.StepStatusCompleted {
			continue
		}
		rounds, err := m.db.GetRoundsByStep(row.RoundsStepID())
		if err != nil {
			return fmt.Errorf("read the carried review rounds: %w", err)
		}
		var recordedRepo, recordedGlobal []byte
		for _, round := range rounds {
			if len(round.RepoConfigYAML) > 0 {
				recordedRepo = round.RepoConfigYAML
			}
			if len(round.GlobalConfigYAML) > 0 {
				recordedGlobal = round.GlobalConfigYAML
			}
		}
		if recordedRepo == nil {
			return fmt.Errorf("the carried review recorded no repository configuration to compare, so rerun the pipeline instead")
		}
		changed, err := changedSettings(recordedRepo, currentYAML)
		if err != nil {
			return err
		}
		if len(changed) > 0 {
			return fmt.Errorf("the repository configuration changed since the carried review ran (%s), so its approval does not carry; rerun the pipeline instead", strings.Join(changed, ", "))
		}
		if recordedGlobal == nil {
			continue
		}
		changed, err = changedSettings(recordedGlobal, globalCfg.SourceYAML)
		if err != nil {
			return err
		}
		if len(changed) > 0 {
			return fmt.Errorf("the global configuration changed since the carried review ran (%s), so its approval does not carry; rerun the pipeline instead", strings.Join(changed, ", "))
		}
	}
	return nil
}

// changedSettings names the top-level settings whose values differ between two
// YAML documents.
func changedSettings(recorded, current []byte) ([]string, error) {
	var before, after map[string]any
	if err := yaml.Unmarshal(recorded, &before); err != nil {
		return nil, fmt.Errorf("read a recorded configuration: %w", err)
	}
	if err := yaml.Unmarshal(current, &after); err != nil {
		return nil, fmt.Errorf("read the current configuration: %w", err)
	}
	var changed []string
	for key, value := range before {
		if next, ok := after[key]; !ok || !reflect.DeepEqual(value, next) {
			changed = append(changed, key)
		}
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	return changed, nil
}

// proveCarriedAgent requires the agent and model that served every carried
// step to still be the ones a new run selects. Each carried row's invocations
// live in the run that first executed it. A run pinned to a Pi profile is
// proven by the profile instead.
func (m *RunManager) proveCarriedAgent(ctx context.Context, source *db.Run, cfg *config.Config, carried []*db.StepResult) error {
	if source.PiProfile != nil {
		return nil
	}
	byRun := map[string][]db.AgentInvocation{}
	resolved := false
	for _, row := range carried {
		origin, err := m.db.GetStepResult(row.RoundsStepID())
		if err != nil || origin == nil {
			return fmt.Errorf("read the origin of carried step %s: %v", row.StepName, err)
		}
		invocations, ok := byRun[origin.RunID]
		if !ok {
			if invocations, err = m.db.GetAgentInvocationsByRun(origin.RunID); err != nil {
				return fmt.Errorf("read the agent invocations of run %s: %w", origin.RunID, err)
			}
			byRun[origin.RunID] = invocations
		}
		for _, inv := range invocations {
			if inv.StepName != string(row.StepName) || inv.ExitStatus != "ok" {
				continue
			}
			if !resolved {
				if err := cfg.ResolveAgent(ctx, exec.LookPath); err != nil {
					return fmt.Errorf("resolve the agent a new run would use: %w", err)
				}
				resolved = true
			}
			expected := cfgForInvocation(cfg, inv)
			name := types.AgentName(inv.Agent)
			if name != expected.Agent && !slices.Contains(expected.Agents, name) {
				return fmt.Errorf("carried step %s ran on agent %s, which is no longer selected (agent is now %s); rerun the pipeline instead", inv.StepName, inv.Agent, expected.Agent)
			}
			if model := expected.AgentProfileFor(name).Model; model != "" && inv.Model != "" && !strings.Contains(inv.Model, model) {
				return fmt.Errorf("carried step %s ran %s on model %s, but agent_config.%s.model is now %s; rerun the pipeline instead", inv.StepName, inv.Agent, inv.Model, inv.Agent, model)
			}
		}
	}
	return nil
}

// cfgForInvocation returns the configuration that would serve inv in a new run,
// selecting a review_agents role from the call's Purpose and Round the way
// newPipelineAgent routes it.
func cfgForInvocation(cfg *config.Config, inv db.AgentInvocation) *config.Config {
	var role, lateRole string
	switch inv.Purpose {
	case "review", "review-coverage":
		role, lateRole = config.RoleReviewer, config.RoleReviewerAfterRound
	case "review-fix":
		role, lateRole = config.RoleFixer, config.RoleFixerAfterRound
	default:
		return cfg
	}
	if late, ok := cfg.ReviewAgents[lateRole]; ok {
		if from := cfg.ReviewAgentTakeoverRound(lateRole); from > 0 && inv.Round >= from {
			return cfg.ForReviewAgent(late)
		}
	}
	if entry, ok := cfg.ReviewAgents[role]; ok {
		return cfg.ForReviewAgent(entry)
	}
	return cfg
}

// proveAtLaunch repeats the configuration proof against the configuration the
// continuation run loaded, then carries the failed run's Review conversation
// into the run's own evidence directory. It refuses, naming the proof, before
// the audit link or any carried step exists.
func (r *retryLaunch) proveAtLaunch(ctx context.Context, m *RunManager, globalCfg *config.GlobalConfig, cfg *config.Config, repoCfg *config.RepoConfig, runID string) error {
	launched := *cfg
	if err := m.proveConfigurationMatches(ctx, r.source, globalCfg, &launched, repoCfg, r.gates, r.checkpoint); err != nil {
		return refuseRetry(r.source.ID, "compatible configuration", "%v", err)
	}
	if err := m.carryReviewConversation(r, cfg, runID); err != nil {
		return refuseRetry(r.source.ID, RetryProofConversation, "%v", err)
	}
	return nil
}

// discardRefusedRetryRun deletes the continuation run row and the evidence
// directory a refused retry created, so the failed run stays the branch's
// latest run and can be retried again once the cause is fixed.
func (m *RunManager) discardRefusedRetryRun(runID string, cfg *config.Config) {
	if err := m.db.DeleteRun(runID); err != nil {
		m.db.UpdateRunError(runID, fmt.Sprintf("discard refused retry: %s", err))
	}
	os.RemoveAll(m.paths.RunEvidenceDir(cfg.Test.Evidence.LocalRoot, runID))
}

// RetryProofConversation names the proof that a carried Review's question
// records are still readable.
const RetryProofConversation = "review conversation"

// carryReviewConversation copies the failed run's review conversation files
// into the continuation run's evidence directory, where the PR step reads
// them. A carried Review that recorded a question whose files can no longer be
// read is refused: the open question would otherwise vanish from the PR.
func (m *RunManager) carryReviewConversation(r *retryLaunch, cfg *config.Config, runID string) error {
	var review *db.StepResult
	for _, row := range r.checkpoint.Steps[:r.checkpoint.Index] {
		if row.StepName == types.StepReview {
			review = row
		}
	}
	if review == nil {
		return nil
	}
	rounds, err := m.db.GetRoundsByStep(review.RoundsStepID())
	if err != nil {
		return fmt.Errorf("read the carried review rounds: %w", err)
	}
	asked := false
	for _, round := range rounds {
		if round.FindingsJSON != nil && pipeline.HasUnansweredReviewQuestion(*round.FindingsJSON) {
			asked = true
		}
	}
	local := cfg.Test.Evidence.LocalRoot
	src := reviewqa.Dir(m.paths.RunEvidenceDir(local, r.source.ID))
	dst := reviewqa.Dir(m.paths.RunEvidenceDir(local, runID))
	entries, err := os.ReadDir(src)
	if err != nil {
		if asked {
			return fmt.Errorf("the carried review asked a question, but its conversation in %s cannot be read (%v), so rerun the pipeline instead", src, err)
		}
		return nil
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create the review conversation directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			return fmt.Errorf("read the carried review conversation: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o644); err != nil {
			return fmt.Errorf("carry the review conversation: %w", err)
		}
	}
	return nil
}
