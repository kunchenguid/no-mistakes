package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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

	output, err := parseStructuredTextOutput(text)
	if err != nil {
		return nil, fmt.Errorf("%s output parse: %w", agentName, err)
	}

	return &Result{Output: output, Text: text, Usage: usage}, nil
}

func parseStructuredTextOutput(text string) (json.RawMessage, error) {
	var output json.RawMessage
	if err := json.Unmarshal([]byte(text), &output); err == nil {
		return output, nil
	} else {
		rawErr := err

		candidates := fencedJSONCandidates(text)
		var parsed []json.RawMessage
		for _, candidate := range candidates {
			var fenced json.RawMessage
			if err := json.Unmarshal([]byte(candidate), &fenced); err == nil {
				parsed = append(parsed, fenced)
			}
		}
		switch len(parsed) {
		case 0:
			// fall through to bare-object scan
		case 1:
			return parsed[0], nil
		default:
			return nil, fmt.Errorf("multiple JSON code fences found in output")
		}

		if bare := lastBareJSONObject(text); bare != nil {
			return bare, nil
		}
		return nil, rawErr
	}
}

// fencedJSONCandidates extracts JSON bodies from ```json ... ``` fences.
// Fence markers may appear anywhere in the text, including glued to the end
// of a preceding line (e.g. "...behavior.```json"), which is a shape real
// codex/GPT-5 output regularly produces.
func fencedJSONCandidates(text string) []string {
	var candidates []string
	rest := text
	for {
		start := indexJSONFenceOpen(rest)
		if start < 0 {
			return candidates
		}
		body := rest[start:]
		end := strings.Index(body, "```")
		if end < 0 {
			return candidates
		}
		candidates = append(candidates, body[:end])
		rest = body[end+3:]
	}
}

// indexJSONFenceOpen returns the byte offset of the content immediately
// following an opening ```json fence (the char after the info line's
// newline), or -1 if no opener exists.
func indexJSONFenceOpen(text string) int {
	search := text
	offset := 0
	for {
		i := strings.Index(search, "```")
		if i < 0 {
			return -1
		}
		after := search[i+3:]
		lineEnd := strings.IndexByte(after, '\n')
		var info string
		if lineEnd < 0 {
			info = after
		} else {
			info = after[:lineEnd]
		}
		if strings.EqualFold(strings.TrimSpace(info), "json") {
			if lineEnd < 0 {
				return offset + i + 3 + len(after)
			}
			return offset + i + 3 + lineEnd + 1
		}
		offset += i + 3
		search = after
	}
}

// lastBareJSONObject scans text for balanced {...} substrings that parse
// as JSON and returns the last one found. This handles models that emit
// reasoning prose followed by a raw JSON answer, with no code fence.
func lastBareJSONObject(text string) json.RawMessage {
	var last json.RawMessage
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		end, ok := scanBalancedObject(text, i)
		if !ok {
			continue
		}
		candidate := text[i:end]
		var obj json.RawMessage
		if err := json.Unmarshal([]byte(candidate), &obj); err == nil {
			last = obj
			i = end - 1
		}
	}
	return last
}

// scanBalancedObject returns the exclusive end index of a brace-balanced
// substring starting at text[start] == '{', or (0, false) if no balanced
// closing brace exists. It respects JSON string literals so braces inside
// strings do not affect the depth count.
func scanBalancedObject(text string, start int) (int, bool) {
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
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
