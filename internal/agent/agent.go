package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Agent is the interface for running AI agent tasks.
type Agent interface {
	Name() string
	Run(ctx context.Context, opts RunOpts) (*Result, error)
	Close() error
}

// RunOpts configures a single agent invocation.
type RunOpts struct {
	Prompt     string
	CWD        string
	JSONSchema json.RawMessage   // structured output schema (optional)
	OnChunk    func(text string) // streaming text callback (optional)
}

// Result holds the output of an agent invocation.
type Result struct {
	Output json.RawMessage // structured output matching JSONSchema
	Text   string          // raw text output
	Usage  TokenUsage
}

// TokenUsage tracks token consumption for an agent invocation.
type TokenUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

func finalizeTextResult(agentName, text string, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	if text == "" {
		return nil, fmt.Errorf("%s returned no text output", agentName)
	}
	if len(schema) == 0 {
		return &Result{Text: text, Usage: usage}, nil
	}

	var output json.RawMessage
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		return nil, fmt.Errorf("%s output parse: %w", agentName, err)
	}

	return &Result{Output: output, Text: text, Usage: usage}, nil
}

// Total returns input + output tokens (the billing-relevant total).
func (u TokenUsage) Total() int {
	return u.InputTokens + u.OutputTokens
}

// Add accumulates another usage into this one.
func (u *TokenUsage) Add(other TokenUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheCreationTokens += other.CacheCreationTokens
}

// New creates an agent by name with the given binary path. extraArgs are user
// CLI flags (from agent_args_override in the global config) that the agent
// injects into the underlying tool's argv ahead of no-mistakes' managed flags.
func New(name types.AgentName, bin string, extraArgs []string) (Agent, error) {
	switch name {
	case types.AgentClaude:
		return &claudeAgent{bin: bin, extraArgs: extraArgs}, nil
	case types.AgentCodex:
		return &codexAgent{bin: bin, extraArgs: extraArgs}, nil
	case types.AgentRovoDev:
		return &rovodevAgent{bin: bin, extraArgs: extraArgs}, nil
	case types.AgentOpenCode:
		return &opencodeAgent{bin: bin, extraArgs: extraArgs}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q; valid options: auto, claude, codex, rovodev, opencode (set 'agent' in ~/.no-mistakes/config.yaml)", name)
	}
}

// NewNoop returns an agent that does nothing. Used for demo mode where
// mock steps handle all logic without calling a real agent.
func NewNoop() Agent { return &noopAgent{} }

type noopAgent struct{}

func (n *noopAgent) Name() string                                      { return "noop" }
func (n *noopAgent) Run(_ context.Context, _ RunOpts) (*Result, error) { return &Result{}, nil }
func (n *noopAgent) Close() error                                      { return nil }
