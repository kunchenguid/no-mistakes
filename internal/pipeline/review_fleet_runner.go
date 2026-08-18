package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	reviewFleetReadOnlySandbox = "read-only"
	reviewFleetMaxArgBytes     = 4096
)

// reviewFleetSettingsFromConfig projects the trusted global-only config into
// the execution types used by the pipeline. The fixed order is deliberate:
// reviewer completion is concurrent, but configuration and test evidence stay
// deterministic.
func reviewFleetSettingsFromConfig(cfg *config.Config) (*ReviewFleetSettings, error) {
	if cfg == nil {
		return nil, nil
	}
	settings := &ReviewFleetSettings{Enabled: cfg.ReviewFleet.Enabled}
	if !settings.Enabled {
		return settings, nil
	}
	roles := []string{
		config.ReviewFleetRoleTestAdversary,
		config.ReviewFleetRoleCorrectness,
		config.ReviewFleetRoleArchitecture,
		config.ReviewFleetRoleSecurity,
	}
	settings.Reviewers = make([]ReviewProfile, 0, len(roles))
	for _, role := range roles {
		profile, ok := cfg.ReviewFleet.Reviewers[role]
		if !ok {
			return nil, fmt.Errorf("review fleet config is missing reviewer %q", role)
		}
		settings.Reviewers = append(settings.Reviewers, projectReviewFleetProfile(role, profile))
	}
	settings.Consolidator = projectReviewFleetProfile(config.ReviewFleetProfileConsolidator, cfg.ReviewFleet.Consolidator)
	settings.Certifier = projectReviewFleetProfile(config.ReviewFleetProfileCertifier, cfg.ReviewFleet.Certifier)
	settings.CodexProfileArgs = func(profile ReviewProfile) ([]string, error) {
		return cfg.ReviewFleetCodexArgs(profile.Role, profile.SecurityEscalated)
	}
	return settings, nil
}

func projectReviewFleetProfile(role string, profile config.ReviewFleetProfile) ReviewProfile {
	return ReviewProfile{
		Role:               role,
		Model:              profile.Model,
		Reasoning:          profile.ReasoningEffort,
		HighRiskPaths:      append([]string(nil), profile.HighRiskPaths...),
		EscalatedReasoning: profile.EscalatedReasoningEffort,
	}
}

func validateReviewFleetArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("safe Codex profile args are empty")
	}
	total := 0
	for _, arg := range args {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return nil, fmt.Errorf("safe Codex profile args contain control characters")
		}
		total += len(arg)
	}
	if total > reviewFleetMaxArgBytes {
		return nil, fmt.Errorf("safe Codex profile args exceed %d bytes", reviewFleetMaxArgBytes)
	}
	return append([]string(nil), args...), nil
}

func validateReviewFleetIsolation(args []string) ([]string, error) {
	validated, err := validateReviewFleetArgs(args)
	if err != nil {
		return nil, err
	}
	var readOnly, ephemeral, ignoredRules, ignoredUserConfig, suppressedProjectDoc bool
	for i := 0; i < len(validated); i++ {
		arg := validated[i]
		switch {
		case arg == "--dangerously-bypass-approvals-and-sandbox", arg == "--full-auto", arg == "--approve-for-me":
			return nil, fmt.Errorf("review fleet Codex args contain unsafe execution flag %q", arg)
		case arg == "--sandbox" || arg == "-s":
			if i+1 >= len(validated) || validated[i+1] != reviewFleetReadOnlySandbox {
				return nil, fmt.Errorf("review fleet Codex sandbox must be read-only")
			}
			readOnly = true
			i++
		case strings.HasPrefix(arg, "--sandbox="):
			if strings.TrimPrefix(arg, "--sandbox=") != reviewFleetReadOnlySandbox {
				return nil, fmt.Errorf("review fleet Codex sandbox must be read-only")
			}
			readOnly = true
		case arg == "--ephemeral":
			ephemeral = true
		case arg == "--ignore-rules":
			ignoredRules = true
		case arg == "--ignore-user-config":
			ignoredUserConfig = true
		case arg == "-c" || arg == "--config":
			if i+1 >= len(validated) {
				return nil, fmt.Errorf("review fleet Codex config flag is incomplete")
			}
			if strings.TrimSpace(validated[i+1]) == "project_doc_max_bytes=0" {
				suppressedProjectDoc = true
			}
			i++
		case strings.HasPrefix(arg, "-c=") || strings.HasPrefix(arg, "--config="):
			value := arg[strings.IndexByte(arg, '=')+1:]
			if strings.TrimSpace(value) == "project_doc_max_bytes=0" {
				suppressedProjectDoc = true
			}
		}
	}
	if !readOnly || !ephemeral || !ignoredRules || !ignoredUserConfig || !suppressedProjectDoc {
		return nil, fmt.Errorf("review fleet Codex args are missing mandatory read-only isolation controls")
	}
	return validated, nil
}

type reviewProfileRunner struct {
	cfg          *config.Config
	settings     *ReviewFleetSettings
	db           *db.DB
	runID        string
	stepName     types.StepName
	round        func() int
	workDir      string
	evidenceRoot string
	onLifecycle  func(agent.LifecycleEvent)
}

func (e *Executor) newReviewProfileRunner(run *db.Run, stepName types.StepName, round func() int, onLifecycle func(agent.LifecycleEvent)) ReviewProfileRunner {
	if e == nil || e.reviewFleet == nil || !e.reviewFleet.Enabled || e.config == nil || run == nil {
		return nil
	}
	runner := &reviewProfileRunner{
		cfg:          e.config,
		settings:     e.reviewFleet,
		db:           e.db,
		runID:        run.ID,
		stepName:     stepName,
		round:        round,
		workDir:      e.workDir,
		evidenceRoot: e.runEvidenceDir(run.ID),
		onLifecycle:  onLifecycle,
	}
	return runner.Run
}

func (r *reviewProfileRunner) Run(ctx context.Context, profile ReviewProfile, opts agent.RunOpts) (*agent.Result, error) {
	if r == nil || r.cfg == nil || r.settings == nil {
		return nil, fmt.Errorf("review fleet runner is not configured")
	}
	if strings.TrimSpace(opts.CWD) != "" && opts.CWD != r.workDir {
		return nil, fmt.Errorf("review fleet runner refuses a worktree outside the shared read-only checkout")
	}
	// Fleet invocations are always cold, even if a caller accidentally passes
	// session metadata copied from the ordinary review loop.
	opts.Session = nil
	opts.SessionFallback = false
	opts.SessionFallbackReason = ""
	argsFn := r.settings.CodexProfileArgs
	if argsFn == nil {
		return nil, fmt.Errorf("review fleet Codex profile args are not configured")
	}
	args, err := argsFn(profile)
	if err != nil {
		return nil, fmt.Errorf("build review fleet Codex profile %q: %w", profile.Role, err)
	}
	args, err = validateReviewFleetIsolation(args)
	if err != nil {
		return nil, err
	}
	base, err := agent.NewWithOptions(types.AgentCodex, r.cfg.AgentPathFor(types.AgentCodex), args, agent.Options{
		ACPRegistryOverrides:   r.cfg.ACPRegistryOverrides,
		DisableProjectSettings: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create review fleet Codex profile %q: %w", profile.Role, err)
	}
	defer base.Close()
	if err := agent.EnsureGateNeutralized(base); err != nil {
		return nil, fmt.Errorf("neutralize review fleet Codex profile %q: %w", profile.Role, err)
	}
	wrapped := agent.WithSteering(base, r.evidenceRoot)
	wrapped = &gateStepBoundaryAgent{inner: wrapped, phase: r.stepName}
	wrapped = &lifecycleAgent{inner: wrapped, onLifecycle: func(event agent.LifecycleEvent) {
		event.Agent = profile.Role + "/" + event.Agent
		if event.Message != "" {
			event.Message = profile.Role + ": " + event.Message
		}
		if r.onLifecycle != nil {
			r.onLifecycle(event)
		}
	}}
	wrapped = &perfRecordingAgent{inner: wrapped, db: r.db, runID: r.runID, stepName: r.stepName, round: r.round}
	opts.CWD = r.workDir
	return wrapped.Run(ctx, opts)
}
