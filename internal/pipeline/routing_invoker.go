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
	types.PurposeInitialReview: true,
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
	newAgent agentFactory
}

func newRoutingInvoker(legacy agent.Invoker, routing config.RoutingConfig, journal agent.InvocationJournal) *routingInvoker {
	return &routingInvoker{
		legacy:  legacy,
		routing: routing,
		journal: journal,
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

	// Resolve the Candidate before any launch. Missing or unknown routing data
	// fails closed here so no model process ever starts on a bad route.
	profiles, err := ri.routing.ResolveRoute(request.Purpose)
	if err != nil {
		return nil, fmt.Errorf("resolve route for %q: %w", request.Purpose, err)
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("route for %q resolved no profile", request.Purpose)
	}
	// Initial review always uses the first (strong) tier; an initially
	// high-risk result records risk but never escalates the discovery tier.
	tier := 0
	profile := profiles[tier]
	if len(profile.Candidates) == 0 {
		return nil, fmt.Errorf("profile %q has no candidate", profile.Name)
	}
	candidateIndex := 0
	candidate := profile.Candidates[candidateIndex]

	runnerSpec, ok := ri.routing.Runners[candidate.Runner]
	if !ok {
		return nil, fmt.Errorf("candidate runner %q is not declared", candidate.Runner)
	}
	agentName, err := candidate.Runner.AgentName()
	if err != nil {
		return nil, err
	}
	nativeAgent, err := ri.newAgent(agentName, runnerSpec.Executable)
	if err != nil {
		return nil, fmt.Errorf("construct %q runner: %w", candidate.Runner, err)
	}
	defer nativeAgent.Close()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	attemptID, err := ri.journal.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      request.Purpose,
		Role:         definition.Role,
		Scope:        request.Scope,
		CandidateKey: candidateKey(profile.Name, candidateIndex, candidate),
		Candidate: types.InvocationCandidate{
			Profile:        string(profile.Name),
			Tier:           tier,
			CandidateIndex: candidateIndex,
			Runner:         candidate.Runner,
			Model:          candidate.Model,
			Effort:         candidate.Effort,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("record routed invocation start: %w", err)
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
	// Only a classified operational failure carries a provider failure domain;
	// malformed output or a bad review never opens a provider circuit.
	if terminal.Outcome == types.InvocationOutcomeFailed {
		var opErr *agent.OperationalError
		if errors.As(runErr, &opErr) {
			if domain, derr := candidate.Runner.FailureDomain(); derr == nil {
				terminal.FailureDomain = domain
			}
		}
	}
	if journalErr := ri.journal.FinishInvocationAttempt(attemptID, terminal); journalErr != nil {
		journalErr = fmt.Errorf("record routed invocation terminal: %w", journalErr)
		if runErr != nil {
			return result, errors.Join(runErr, journalErr)
		}
		return result, journalErr
	}
	return result, runErr
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
