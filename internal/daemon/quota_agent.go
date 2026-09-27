package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/quota"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// quotaAutoPipelineAgent builds a run's pipeline agent under `agent: quota-auto`
// and returns the step-boundary hook that keeps it routable.
//
// The shape of the run is unchanged from every other selection mode: one primary
// agent wrapped with the operator's review-loop roles, and the candidate harnesses
// chained as launch-failure fallbacks. What quota adds is WHICH harness leads that
// chain: the run's opening selection is made here, from live quota evidence, and
// the returned hook re-checks it at every step boundary so a provider that runs
// dry mid-run is replaced instead of stranding the steps behind it.
func (m *RunManager) quotaAutoPipelineAgent(
	ctx context.Context,
	cfg *config.Config,
	runID string,
	firstStep types.StepName,
	evidenceRoot string,
	lookPath func(string) (string, error),
	environment runenv.Overlay,
) (agent.Agent, func(context.Context, types.StepName) error, error) {
	candidates, err := quotaCandidates(cfg)
	if err != nil {
		return nil, nil, err
	}
	build := func(name types.AgentName) (agent.Agent, error) {
		return newAgentChain(cfg, name, evidenceRoot, environment)
	}
	switcher, err := quota.NewSwitchingAgent(quota.Options{
		Candidates: candidates,
		Reader:     newQuotaEvidenceReader(cfg.QuotaAXIPath),
		Budgets: quota.Budgets{
			Default: cfg.AgentTimeout,
			Review:  cfg.ReviewAgentTimeout,
			Test:    cfg.TestAgentTimeout,
		},
		Profiles: cfg.AgentConfig,
		Build:    build,
		Record:   m.agentSelectionRecorder(runID),
		// The trusted opt-out refuses unverified harnesses at construction; a
		// later switch must be refused the same way, or quota could seat a
		// harness the opt-out was there to keep out.
		RequireNeutralized: cfg.DisableProjectSettings,
		Logf: func(line string) {
			slog.Info("quota-auto agent selection", "run_id", runID, "selection", line)
		},
	})
	if err != nil {
		return nil, nil, err
	}
	if err := switcher.Start(ctx, firstStep); err != nil {
		return nil, nil, err
	}
	wrapped, err := withReviewRolesAgent(ctx, cfg, switcher, evidenceRoot, lookPath, environment)
	if err != nil {
		_ = switcher.Close()
		return nil, nil, err
	}
	return wrapped, switcher.Reassess, nil
}

// newQuotaEvidenceReader resolves the tool that supplies routing evidence. It is
// a variable because the decision logic is worth testing against recorded
// evidence; the spawn itself is covered by the e2e suite, which owns process
// boundaries.
var newQuotaEvidenceReader = func(path string) quota.Reader {
	return quota.NewAXIReader(path)
}

// quotaCandidates parses the validated candidate list into the routing
// vocabulary. Load-time validation already refused malformed entries, so a
// failure here means the config was built in code rather than read from disk.
func quotaCandidates(cfg *config.Config) ([]quota.Candidate, error) {
	candidates := make([]quota.Candidate, 0, len(cfg.AgentCandidates))
	for _, raw := range cfg.AgentCandidates {
		candidate, err := quota.ParseCandidate(raw)
		if err != nil {
			return nil, fmt.Errorf("agent_candidates: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("agent: %s needs agent_candidates", types.AgentQuotaAuto)
	}
	return candidates, nil
}

// agentSelectionRecorder persists routing decisions on the run.
//
// Recording is best-effort, like the run's other non-verdict state: a database
// write failure must not fail a run that is otherwise routing correctly, and the
// decision itself still reaches the daemon log.
func (m *RunManager) agentSelectionRecorder(runID string) func(context.Context, quota.Record) {
	return func(_ context.Context, record quota.Record) {
		selection := db.AgentSelection{
			RunID:     runID,
			Step:      string(record.Step),
			Reason:    record.Reason,
			Agent:     string(record.Decision.Candidate.Agent),
			Provider:  record.Decision.EvidenceProvider,
			Model:     record.Decision.Row.ID,
			Evidence:  record.EvidencePayload(),
			CreatedAt: time.Now().Unix(),
		}
		if !record.Decision.GeneratedAt.IsZero() {
			generated := record.Decision.GeneratedAt.Unix()
			selection.ReportAt = &generated
		}
		if err := m.db.RecordAgentSelection(selection); err != nil {
			slog.Warn("record quota-auto agent selection", "run_id", runID, "error", err)
		}
	}
}

// nextStepForRun returns the first step that still has to run, which is the step
// a starting or resuming selection has to satisfy.
//
// A fresh run answers with its first step. A run resumed after a crash answers
// with the step it is about to re-enter, so recovery does not demand a candidate
// that could serve a step this run already finished. An unreadable step plan
// answers with the first step, which is the conservative end of the same
// question.
func nextStepForRun(database *db.DB, runID string, plan []pipeline.Step) types.StepName {
	if len(plan) == 0 {
		return ""
	}
	results, err := database.GetStepsByRun(runID)
	if err != nil {
		return plan[0].Name()
	}
	for index, step := range plan {
		if index >= len(results) || results[index].StepName != step.Name() {
			return plan[0].Name()
		}
		switch results[index].Status {
		case types.StepStatusCompleted, types.StepStatusSkipped:
			continue
		}
		return step.Name()
	}
	return plan[0].Name()
}
