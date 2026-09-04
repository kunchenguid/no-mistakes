package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const (
	PonytailHandoffProtocol = "no-mistakes.ponytail-handoff.v1"
	ponytailMode            = "full"
)

const ponytailOperatingContext = `Ponytail full operating context:
- Understand the task and trace the code path before editing.
- Prefer, in order: no change, existing code, the standard library, native platform behavior, an already-installed dependency, then the minimum new code.
- Fix root causes at the shared seam instead of patching each caller.
- Add no speculative abstraction, dependency, boilerplate, or configuration.
- Prefer deletion and boring code, while preserving required validation, error handling, security, accessibility, and focused tests.
- Keep this mode active for the whole project turn.
`

type ponytailAcknowledgment struct {
	Protocol     string `json:"protocol"`
	Mode         string `json:"mode"`
	Challenge    string `json:"challenge"`
	Acknowledged bool   `json:"acknowledged"`
}

type ponytailHandoffAgent struct {
	inner Agent
}

// WithPonytailHandoff requires an exact structured acknowledgment before each
// project invocation. Fallback members are wrapped individually so a provider
// switch cannot reuse another provider's acknowledgment.
func WithPonytailHandoff(a Agent) Agent {
	if a == nil {
		return nil
	}
	switch current := a.(type) {
	case *ponytailHandoffAgent:
		return a
	case steeredAgent:
		current.Agent = WithPonytailHandoff(current.Agent)
		return current
	case *fallbackAgent:
		wrapped := make([]Agent, len(current.agents))
		for i, candidate := range current.agents {
			wrapped[i] = WithPonytailHandoff(candidate)
		}
		return NewFallback(wrapped)
	default:
		return &ponytailHandoffAgent{inner: a}
	}
}

func (a *ponytailHandoffAgent) Name() string { return a.inner.Name() }

func (a *ponytailHandoffAgent) Close() error { return a.inner.Close() }

func (a *ponytailHandoffAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	challengeBytes := make([]byte, 16)
	if _, err := rand.Read(challengeBytes); err != nil {
		return nil, fmt.Errorf("ponytail handoff for agent %q: could not create acknowledgment challenge; retry the run", a.Name())
	}
	challenge := hex.EncodeToString(challengeBytes)
	handoffDir, err := os.MkdirTemp("", "no-mistakes-ponytail-handoff-")
	if err != nil {
		return nil, fmt.Errorf("ponytail handoff for agent %q: could not create an isolated workspace; retry the run", a.Name())
	}
	defer os.RemoveAll(handoffDir)
	handoffOpts := opts
	handoffOpts.Prompt = ponytailHandshakePrompt(challenge)
	handoffOpts.CWD = handoffDir
	handoffOpts.JSONSchema = ponytailAcknowledgmentSchema(challenge)
	handoffOpts.OnChunk = nil
	handoffOpts.OnLifecycle = nil
	handoffOpts.OnAttempt = nil
	handoffOpts.Env = nil
	handoffOpts.Session = nil
	handoffOpts.SessionFallback = false
	handoffOpts.SessionFallbackReason = ""
	handoffOpts.Purpose = "ponytail-handoff"
	handoffOpts.Workload = nil

	ackResult, err := a.inner.Run(ctx, handoffOpts)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isAgentUnavailableError(err) {
			return nil, fmt.Errorf("ponytail handoff for agent %q start: agent unavailable; install or repair the configured agent, then rerun", a.Name())
		}
		return nil, fmt.Errorf("ponytail handoff for agent %q failed before project work; verify Ponytail full can be activated and acknowledged, then rerun", a.Name())
	}
	ack, valid := decodePonytailAcknowledgment(ackResult)
	if !valid ||
		ack.Protocol != PonytailHandoffProtocol || ack.Mode != ponytailMode ||
		ack.Challenge != challenge || !ack.Acknowledged {
		return nil, fmt.Errorf("ponytail handoff for agent %q returned an invalid acknowledgment before project work; verify Ponytail full support, then rerun", a.Name())
	}

	opts.Prompt = ponytailProjectPrompt(challenge) + opts.Prompt
	return a.inner.Run(ctx, opts)
}

func decodePonytailAcknowledgment(result *Result) (ponytailAcknowledgment, bool) {
	var ack ponytailAcknowledgment
	if result == nil {
		return ack, false
	}
	decoder := json.NewDecoder(bytes.NewReader(result.Output))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ack) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ack, false
	}
	return ack, true
}

func (a *ponytailHandoffAgent) SupportsSessionResume() bool {
	return SupportsSessionResume(a.inner)
}

func (a *ponytailHandoffAgent) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(a.inner, provider)
}

func (a *ponytailHandoffAgent) ReportsAgentAttempts() bool {
	return ReportsAgentAttempts(a.inner)
}

func (a *ponytailHandoffAgent) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(a.inner)
}

func ponytailHandshakePrompt(challenge string) string {
	return "Activate and acknowledge the following operating context before any project work. " +
		"Do not inspect files, call tools, or perform the project task during this handshake.\n\n" +
		ponytailOperatingContext + "\nAcknowledgment protocol: " + PonytailHandoffProtocol +
		"\nMode: " + ponytailMode + "\nChallenge: " + challenge +
		"\nReturn only the required acknowledgment object. If you cannot adopt this context, do not acknowledge it."
}

func ponytailProjectPrompt(challenge string) string {
	return "Ponytail handoff: protocol=" + PonytailHandoffProtocol + " mode=" + ponytailMode +
		" acknowledged_challenge=" + challenge + "\n" + ponytailOperatingContext + "\n"
}

func ponytailAcknowledgmentSchema(challenge string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"protocol":{"type":"string","enum":[%q]},"mode":{"type":"string","enum":[%q]},"challenge":{"type":"string","enum":[%q]},"acknowledged":{"type":"boolean","enum":[true]}},"required":["protocol","mode","challenge","acknowledged"],"additionalProperties":false}`, PonytailHandoffProtocol, ponytailMode, challenge))
}
