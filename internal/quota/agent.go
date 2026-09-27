package quota

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Decision reasons recorded against a selection, so a reader can tell a run's
// opening choice from a mid-run switch without reading the pipeline's logs.
const (
	// ReasonRunStart marks the selection a run began with.
	ReasonRunStart = "run-start"
	// ReasonStepBoundary marks a switch made at a step boundary because the
	// harness in use had stopped being viable for the step ahead.
	ReasonStepBoundary = "step-boundary"
)

// Builder constructs the harness agent for one candidate. It is supplied by the
// caller because only the daemon knows the run's harness arguments, evidence
// root and environment.
type Builder func(name types.AgentName) (agent.Agent, error)

// Record is one routing decision as it is persisted on the run.
type Record struct {
	Step      types.StepName
	Reason    string
	Decision  Decision
	Generated time.Time
	// ReadError is set when the evidence itself could not be read. A run-start
	// selection never records one (the run fails first); a boundary may, because
	// a boundary keeps the harness it already has rather than failing the run
	// over a tool that went missing mid-run.
	ReadError string
}

// Options configures a SwitchingAgent.
type Options struct {
	Candidates []Candidate
	Reader     Reader
	Budgets    Budgets
	// Profiles is the operator's per-harness pin (agent_config), which decides
	// what the class gate attests against.
	Profiles map[string]agentcfg.Profile
	Build    Builder
	// Record receives every decision this agent makes. It is optional; a nil
	// recorder keeps selection working with nothing persisted.
	Record func(context.Context, Record)
	// RequireNeutralized refuses to switch to a harness that does not neutralize
	// the target repository's project instruction files. It mirrors the daemon's
	// disable_project_settings fail-closed check, which is applied to the harness
	// present at construction: without this, a later quota switch could seat a
	// harness the opt-out was supposed to refuse.
	RequireNeutralized bool
	Logf               func(string)
}

// SwitchingAgent is a run's pipeline agent under quota-auto.
//
// It is an agent.Agent that delegates to the harness the evidence selected, and
// it can re-select at a step boundary. Step boundaries are the only point at
// which the harness may change: within a step the executor reuses the same
// delegate across review rounds and fix turns, where a harness swap would drop
// the harness-native session those turns are built on.
type SwitchingAgent struct {
	candidates         []Candidate
	reader             Reader
	budgets            Budgets
	profiles           map[string]agentcfg.Profile
	build              Builder
	record             func(context.Context, Record)
	requireNeutralized bool
	logLine            func(string)

	mu      sync.Mutex
	current agent.Agent
	// startStep is the step the opening selection was made for. The executor
	// reassesses that same step immediately afterwards, and re-reading evidence for
	// a decision taken moments ago would spend a vendor read to reach the answer it
	// already has. It is cleared on first use so a later restart of the same step
	// is reassessed normally.
	startStep types.StepName
}

// NewSwitchingAgent validates the wiring and returns the agent. It builds
// nothing: the run-start selection is what decides which harness exists.
func NewSwitchingAgent(opts Options) (*SwitchingAgent, error) {
	if len(opts.Candidates) == 0 {
		return nil, fmt.Errorf("quota-auto needs at least one agent candidate")
	}
	if opts.Build == nil {
		return nil, fmt.Errorf("quota-auto needs a harness builder")
	}
	return &SwitchingAgent{
		candidates:         opts.Candidates,
		reader:             opts.Reader,
		budgets:            opts.Budgets,
		profiles:           opts.Profiles,
		build:              opts.Build,
		record:             opts.Record,
		requireNeutralized: opts.RequireNeutralized,
		logLine:            opts.Logf,
	}, nil
}

// Candidates returns the configured candidate list in order.
func (a *SwitchingAgent) Candidates() []Candidate {
	out := make([]Candidate, len(a.candidates))
	copy(out, a.candidates)
	return out
}

// Name reports the harness currently in use.
func (a *SwitchingAgent) Name() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil {
		return ""
	}
	return a.current.Name()
}

// Current returns the harness currently in use, or "" when none is selected.
func (a *SwitchingAgent) Current() types.AgentName {
	return types.AgentName(a.Name())
}

// Start makes the run's opening selection and builds its harness. A run whose
// candidates are all unmeasurable or exhausted fails here with the per-candidate
// report rather than starting on a guess.
func (a *SwitchingAgent) Start(ctx context.Context, step types.StepName) error {
	decision, err := a.selectFor(ctx, step, "")
	if err != nil {
		return err
	}
	delegate, err := a.build(decision.Candidate.Agent)
	if err != nil {
		return fmt.Errorf("create quota-auto agent %s: %w", decision.Candidate.Agent, err)
	}
	a.mu.Lock()
	a.current = delegate
	a.startStep = step
	a.mu.Unlock()
	a.logf("quota-auto %s: %s", ReasonRunStart, decision.Summary())
	a.persist(ctx, Record{Step: step, Reason: ReasonRunStart, Decision: decision})
	return nil
}

// Reassess re-checks the run's harness before a step and switches when the
// harness in use can no longer serve it. Callers must not reassess while an
// invocation is in flight - the executor calls it at a step boundary, where
// nothing is running - because a switch closes the harness it replaces.
//
// The rules here are the ones that keep a mid-run switch from being worse than
// the problem it solves. A harness that still passes every gate is kept: the
// switch costs the run its session and its prompt continuity, so it is spent
// only when the step ahead cannot run on what the run has. Evidence that cannot
// be read is not fail-closed at a boundary - the run keeps the harness it
// already has and says so - because the failure mode that motivated this feature
// is a run that could not finish, and refusing to continue on a missing tool
// would strand a run that is otherwise able to work. Nothing eligible and the
// harness in use also ineligible is the one case that fails the run, with the
// report.
func (a *SwitchingAgent) Reassess(ctx context.Context, step types.StepName) error {
	a.mu.Lock()
	if step != "" && step == a.startStep {
		a.startStep = ""
		a.mu.Unlock()
		return nil
	}
	current := a.current
	a.mu.Unlock()
	if current == nil {
		return fmt.Errorf("quota-auto has no agent selected")
	}
	currentName := types.AgentName(current.Name())

	decision, err := a.selectFor(ctx, step, currentName)
	if err != nil {
		if _, ok := err.(*NoCandidateError); ok {
			return err
		}
		// The evidence could not be read. Keep the harness in use: this is a
		// boundary check, not the run's opening choice.
		a.logf("quota-auto kept %s for %s: %v", currentName, step, err)
		return nil
	}
	if decision.Kept {
		return nil
	}

	delegate, buildErr := a.build(decision.Candidate.Agent)
	if buildErr != nil {
		return fmt.Errorf("create quota-auto agent %s: %w", decision.Candidate.Agent, buildErr)
	}
	if !a.admitsSwitch(delegate, decision.Candidate.Agent) {
		_ = delegate.Close()
		return fmt.Errorf("quota-auto cannot switch to %s: %s", decision.Candidate.Agent, neutralizationRefusal(decision.Candidate.Agent))
	}

	a.mu.Lock()
	previous := a.current
	a.current = delegate
	a.mu.Unlock()
	if previous != nil {
		if closeErr := previous.Close(); closeErr != nil {
			slog.Warn("close superseded quota-auto agent", "agent", previous.Name(), "error", closeErr)
		}
	}
	a.logf("quota-auto %s: %s", ReasonStepBoundary, decision.Summary())
	a.persist(ctx, Record{Step: step, Reason: ReasonStepBoundary, Decision: decision})
	return nil
}

// Run delegates to the harness in use. A run whose harness has not been selected
// yet is a programming error, not a quota outcome, and says so.
func (a *SwitchingAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.mu.Lock()
	current := a.current
	a.mu.Unlock()
	if current == nil {
		return nil, fmt.Errorf("quota-auto has no agent selected")
	}
	return current.Run(ctx, opts)
}

// Close closes the harness in use.
func (a *SwitchingAgent) Close() error {
	a.mu.Lock()
	current := a.current
	a.current = nil
	a.mu.Unlock()
	if current == nil {
		return nil
	}
	return current.Close()
}

// SupportsSessionResume mirrors the harness in use.
func (a *SwitchingAgent) SupportsSessionResume() bool {
	a.mu.Lock()
	current := a.current
	a.mu.Unlock()
	return agent.SupportsSessionResume(current)
}

// SupportsSessionProvider reports whether any harness this run may route to owns
// a session minted by provider.
//
// The answer is deliberately about the candidate SET rather than the harness in
// use: a run that switched harnesses must still be able to resume a session the
// previously selected harness minted when quota routes back to it, and the
// daemon's recovery check must not refuse a run whose recorded session belongs to
// a candidate that is selectable but not currently selected. A harness that
// cannot resume a session ignores the reference and runs cold, which is the
// documented degradation rather than a correctness failure.
func (a *SwitchingAgent) SupportsSessionProvider(provider string) bool {
	if provider == "" {
		return false
	}
	a.mu.Lock()
	current := a.current
	a.mu.Unlock()
	if agent.SupportsSessionProvider(current, provider) {
		return true
	}
	for _, candidate := range a.candidates {
		if string(candidate.Agent) == provider {
			return true
		}
	}
	return false
}

// ReportsAgentAttempts mirrors the harness in use.
func (a *SwitchingAgent) ReportsAgentAttempts() bool {
	a.mu.Lock()
	current := a.current
	a.mu.Unlock()
	return agent.ReportsAgentAttempts(current)
}

// NeutralizesGateInstructions mirrors the harness in use. It fails closed on an
// unselected agent, exactly as the wrapped capability does.
func (a *SwitchingAgent) NeutralizesGateInstructions() bool {
	a.mu.Lock()
	current := a.current
	a.mu.Unlock()
	return agent.NeutralizesGateInstructions(current)
}

// selectFor reads evidence and runs the gates for one step.
func (a *SwitchingAgent) selectFor(ctx context.Context, step types.StepName, current types.AgentName) (Decision, error) {
	if a.reader == nil {
		return Decision{}, fmt.Errorf("quota-auto has no quota evidence reader")
	}
	report, err := a.reader.Read(ctx)
	if err != nil {
		return Decision{}, err
	}
	return Select(a.candidates, report, Request{
		Step:    step,
		Class:   StepClass(step),
		Budget:  a.budgets.For(step),
		Current: current,
		Profile: a.profiles,
	})
}

// admitsSwitch applies the opt-out's fail-closed rule to a harness this run was
// not constructed with.
func (a *SwitchingAgent) admitsSwitch(delegate agent.Agent, name types.AgentName) bool {
	if !a.requireNeutralized {
		return true
	}
	return agent.NeutralizesGateInstructions(delegate)
}

func neutralizationRefusal(name types.AgentName) string {
	return fmt.Sprintf("harness %s does not neutralize the target repository's project instruction files, which disable_project_settings requires", name)
}

func (a *SwitchingAgent) persist(ctx context.Context, record Record) {
	if a.record == nil {
		return
	}
	if record.Generated.IsZero() {
		record.Generated = time.Now().UTC()
	}
	a.record(ctx, record)
}

func (a *SwitchingAgent) logf(format string, args ...any) {
	if a.logLine == nil {
		return
	}
	a.logLine(fmt.Sprintf(format, args...))
}
