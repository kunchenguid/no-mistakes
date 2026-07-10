package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// routedPurposes is the set of Purposes migrated to the routing system. Every
// other Purpose delegates to the legacy invoker unchanged, so this ticket
// routes only the initial review while the rest of the pipeline is untouched.
var routedPurposes = map[types.Purpose]bool{
	types.PurposeInitialReview:               true,
	types.PurposeStructuredFindingRepair:     true,
	types.PurposeNormalAggregateVerification: true,
}

// agentFactory builds a fresh native agent for a runner executable. It is a
// field so tests can inject a recording agent without launching a real binary.
type agentFactory func(name types.AgentName, executable string) (agent.Agent, error)

// routingInvoker resolves a migrated Purpose to a normalized Candidate, launches
// it as a fresh native process, and records the full Candidate attempt. Every
// unmigrated Purpose, and any invocation while routing is unconfigured, falls
// through to the legacy invoker unchanged.
type routingInvoker struct {
	legacy   agent.Invoker
	routing  config.RoutingConfig
	journal  agent.InvocationJournal
	circuits *providerCircuits
	newAgent agentFactory
}

func newRoutingInvoker(legacy agent.Invoker, routing config.RoutingConfig, journal agent.InvocationJournal, circuits *providerCircuits) *routingInvoker {
	return &routingInvoker{
		legacy:   legacy,
		routing:  routing,
		journal:  journal,
		circuits: circuits,
		newAgent: func(name types.AgentName, executable string) (agent.Agent, error) {
			return agent.New(name, executable, nil)
		},
	}
}

func (ri *routingInvoker) Invoke(ctx context.Context, request agent.InvocationRequest) (*agent.Result, error) {
	if ri.routing.IsZero() || !routedPurposes[request.Purpose] {
		return ri.legacy.Invoke(ctx, request)
	}
	return ri.invokeRouted(ctx, request)
}

func (ri *routingInvoker) invokeRouted(ctx context.Context, request agent.InvocationRequest) (*agent.Result, error) {
	if err := agent.ValidateInvocationRequest(request); err != nil {
		return nil, err
	}
	if ri.journal == nil {
		return nil, fmt.Errorf("routing invoker journal is nil")
	}
	definition, _ := types.PurposeDefinitionFor(request.Purpose)

	// Resolve the Route before any launch. Missing or unknown routing data
	// fails closed here so no model process ever starts on a bad route.
	profiles, err := ri.routing.ResolveRoute(request.Purpose)
	if err != nil {
		return nil, fmt.Errorf("resolve route for %q: %w", request.Purpose, err)
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("route for %q resolved no profile", request.Purpose)
	}
	// The requested Tier selects which Profile in the Route to launch, so a
	// repair coordinator can escalate through the cascade. Out-of-range tiers
	// fail closed rather than silently clamping to a weaker or stronger Profile.
	tier := request.Tier
	if tier < 0 || tier >= len(profiles) {
		return nil, fmt.Errorf("purpose %q has no tier %d in a %d-profile route", request.Purpose, tier, len(profiles))
	}
	profile := profiles[tier]
	if len(profile.Candidates) == 0 {
		return nil, fmt.Errorf("profile %q has no candidate", profile.Name)
	}

	// Try Candidates in provider-preference order, one at a time (providers are
	// never raced). Skip any whose provider circuit is already open, fail over
	// to the backup family on a classified operational failure, and fail closed
	// when every Candidate is unavailable rather than weakening the Profile.
	var operationalFailure error
	for index, candidate := range profile.Candidates {
		domain, derr := candidate.Runner.FailureDomain()
		if derr != nil {
			return nil, derr
		}
		if ri.circuits.isOpen(domain) {
			if err := ri.recordSkip(request, definition, profile, tier, index, candidate, domain); err != nil {
				return nil, err
			}
			continue
		}
		result, runErr, fatalErr := ri.launchCandidate(ctx, request, definition, profile, tier, index, candidate, domain)
		if fatalErr != nil {
			return nil, fatalErr
		}
		if runErr == nil {
			return result, nil
		}
		// A launched Candidate failed. Only a classified operational failure
		// (recorded after adapter retries) opens the circuit and fails over to
		// the backup family; every other failure — malformed output, a bad
		// review, cancellation — is returned so the caller sees the real cause.
		var opErr *agent.OperationalError
		if errors.As(runErr, &opErr) {
			operationalFailure = runErr
			continue
		}
		return result, runErr
	}
	if operationalFailure != nil {
		return nil, fmt.Errorf("profile %q exhausted every candidate after operational failures: %w", profile.Name, operationalFailure)
	}
	return nil, fmt.Errorf("profile %q has no available candidate: all provider circuits are open", profile.Name)
}

// startFor builds the immutable start fact for one Candidate attempt.
func startFor(request agent.InvocationRequest, definition types.PurposeDefinition, profile config.Profile, tier, index int, candidate config.Candidate) types.InvocationAttemptStart {
	return types.InvocationAttemptStart{
		Purpose:      request.Purpose,
		Role:         definition.Role,
		Scope:        request.Scope,
		CandidateKey: candidateKey(profile.Name, index, candidate),
		Candidate: types.InvocationCandidate{
			Profile:        string(profile.Name),
			Tier:           tier,
			CandidateIndex: index,
			Runner:         candidate.Runner,
			Model:          candidate.Model,
			Effort:         candidate.Effort,
		},
	}
}

// recordSkip records a Candidate an open provider circuit skipped without
// launching, preserving the skipped-domain decision in immutable history.
func (ri *routingInvoker) recordSkip(request agent.InvocationRequest, definition types.PurposeDefinition, profile config.Profile, tier, index int, candidate config.Candidate, domain types.FailureDomain) error {
	attemptID, err := ri.journal.StartInvocationAttempt(startFor(request, definition, profile, tier, index, candidate))
	if err != nil {
		return fmt.Errorf("record skipped candidate start: %w", err)
	}
	if err := ri.journal.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{
		Outcome:       types.InvocationOutcomeSkipped,
		FailureDomain: domain,
	}); err != nil {
		return fmt.Errorf("record skipped candidate terminal: %w", err)
	}
	return nil
}

// launchCandidate records the start fact, launches the Candidate as a fresh
// native process, and appends its terminal. A classified operational failure
// opens the Candidate's provider circuit. The third return is a fatal error
// that aborts the whole cascade (bad routing data or a journal failure); the
// second is the Candidate's own run error, which the caller uses to decide
// whether to fail over.
func (ri *routingInvoker) launchCandidate(ctx context.Context, request agent.InvocationRequest, definition types.PurposeDefinition, profile config.Profile, tier, index int, candidate config.Candidate, domain types.FailureDomain) (*agent.Result, error, error) {
	runnerSpec, ok := ri.routing.Runners[candidate.Runner]
	if !ok {
		return nil, nil, fmt.Errorf("candidate runner %q is not declared", candidate.Runner)
	}
	agentName, err := candidate.Runner.AgentName()
	if err != nil {
		return nil, nil, err
	}
	nativeAgent, err := ri.newAgent(agentName, runnerSpec.Executable)
	if err != nil {
		return nil, nil, fmt.Errorf("construct %q runner: %w", candidate.Runner, err)
	}
	defer nativeAgent.Close()

	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	attemptID, err := ri.journal.StartInvocationAttempt(startFor(request, definition, profile, tier, index, candidate))
	if err != nil {
		return nil, nil, fmt.Errorf("record routed invocation start: %w", err)
	}

	payload := request.Payload
	payload.Model = candidate.Model
	payload.Effort = candidate.Effort

	startedAt := time.Now()
	result, runErr := nativeAgent.Run(ctx, payload)

	terminal := types.InvocationAttemptTerminal{
		Outcome:    routedOutcome(ctx, runErr),
		DurationMS: time.Since(startedAt).Milliseconds(),
	}
	if result != nil {
		terminal.InputTokens = int64(result.Usage.InputTokens)
		terminal.OutputTokens = int64(result.Usage.OutputTokens)
		terminal.CacheReadTokens = int64(result.Usage.CacheReadTokens)
		terminal.CacheCreationTokens = int64(result.Usage.CacheCreationTokens)
	}
	// Only a classified operational failure carries a provider failure domain
	// and opens the circuit; malformed output or a bad review never does.
	var opErr *agent.OperationalError
	if errors.As(runErr, &opErr) {
		terminal.FailureDomain = domain
		ri.circuits.markOpen(domain)
	}
	if journalErr := ri.journal.FinishInvocationAttempt(attemptID, terminal); journalErr != nil {
		journalErr = fmt.Errorf("record routed invocation terminal: %w", journalErr)
		if runErr != nil {
			return result, errors.Join(runErr, journalErr), nil
		}
		return result, nil, journalErr
	}
	return result, runErr, nil
}

func routedOutcome(ctx context.Context, err error) types.InvocationOutcome {
	if err == nil {
		return types.InvocationOutcomeSucceeded
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return types.InvocationOutcomeCancelled
	}
	return types.InvocationOutcomeFailed
}

// candidateKey is the human-readable Candidate identity recorded alongside the
// structured routing facts.
func candidateKey(profile config.ProfileName, index int, candidate config.Candidate) string {
	return fmt.Sprintf("%s:%d:%s", profile, index, candidate.Runner)
}
