package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

var errOpencodeThinkingToolChoiceConflict = errors.New("opencode provider rejects required tool choice while thinking is enabled")

// errOpencodeToolsAlreadyRan annotates a failure whose turn had already
// invoked a tool. The prompt-only fallback re-runs the whole prompt in a
// fresh session, so it must not be taken past this marker.
var errOpencodeToolsAlreadyRan = errors.New("the failed turn already ran tools")

// errOpencodeToolActivityUnknown annotates a failure whose turn could not be
// read at all: no tool part observed, and no complete record of the turn to
// prove none ran. It withholds the same replays as errOpencodeToolsAlreadyRan
// - an unverified turn is not a turn that did nothing - and stays a separate
// sentinel so the surfaced error says which of the two it was.
var errOpencodeToolActivityUnknown = errors.New("could not verify the failed turn ran no tools")

// thinkingConflict builds the fallback trigger, carrying the turn's tool
// evidence. A session.error can arrive at any point in a turn, so the
// conflict is not always detected before the model has acted, and the
// fallback is another fresh session.
func thinkingConflict(evidence opencodeToolEvidence, cause error) error {
	err := errOpencodeThinkingToolChoiceConflict
	if !evidence.replaySafe() {
		err = fmt.Errorf("%w (%w)", err, evidence.marker())
	}
	if cause != nil {
		return fmt.Errorf("%w: %v", err, cause)
	}
	return err
}

var thinkingToolChoiceConflictPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:(?:required|forced)\s+tool[_ ]choice|tool[_ ]choice\s*(?:is\s*)?["']?(?:required|forced)["']?)\s+(?:is\s+)?(?:incompatible with|cannot be combined with|can't be combined with|cannot be used with|can't be used with|not supported (?:with|when))\s+(?:thinking|reasoning)(?:\s+(?:enabled|mode))?`),
	regexp.MustCompile(`(?i)(?:thinking|reasoning)(?:\s+(?:enabled|mode))?\s+(?:is\s+)?(?:incompatible with|cannot be combined with|can't be combined with|cannot be used with|can't be used with|not supported (?:with|when))\s+(?:(?:a|an|the)\s+)?(?:(?:required|forced)\s+tool[_ ]choice|tool[_ ]choice\s*(?:is\s*)?["']?(?:required|forced)["']?)`),
	regexp.MustCompile(`(?i)(?:thinking|reasoning)\s+may not be enabled when\s+tool[_ ]choice\s+forces\s+tool use`),
}

// opencodeAgent starts a persistent HTTP server via `opencode serve`
// and sends requests via REST with SSE streaming.
type opencodeAgent struct {
	bin       string
	extraArgs []string
	// profile is the harness-neutral model/effort selection resolved by
	// internal/agentcfg. `opencode serve` rejects model and variant flags
	// outright, so unlike every other native adapter these two knobs cannot ride
	// argv: they belong to the session-message body (see sendMessage).
	profile agentcfg.Profile
	subprocessContext
	mu     sync.Mutex
	server *managedServer
}

func (a *opencodeAgent) Name() string { return "opencode" }

func (a *opencodeAgent) ReportsAgentAttempts() bool { return true }

func (a *opencodeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "opencode", opts, claudeMaxRetries, classifyOpencodeTransient, a.recoverTransientRetry, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *opencodeAgent) recoverTransientRetry(label string) {
	if label != "connection refused" {
		return
	}
	a.mu.Lock()
	srv := a.server
	a.server = nil
	a.mu.Unlock()
	if srv != nil {
		srv.shutdown()
	}
}

func (a *opencodeAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	result, err := a.runOnceWithFormat(ctx, opts, true)
	if err == nil || len(opts.JSONSchema) == 0 || !errors.Is(err, errOpencodeThinkingToolChoiceConflict) {
		return result, err
	}

	// The fallback is a second attempt in a fresh session, so a turn that
	// already invoked a tool would replay its side effects - and so would one
	// whose tool activity could not be established. Same reasoning as
	// classifyOpencodeTransient, and the same fail-closed answer: report the
	// conflict and let the operator decide.
	if opencodeReplayUnsafe(err) {
		return nil, err
	}

	// OpenCode implements json_schema output as a required StructuredOutput
	// tool call. Some thinking-enabled models reject that combination. Retry
	// once without the native format, while keeping the schema in the prompt
	// and validating the returned JSON against it in finalizeTextResult.
	emitAgentControl(opts, LifecycleEvent{
		Agent:   a.Name(),
		Phase:   LifecyclePhaseFallback,
		Message: "opencode starting a fresh prompt-only structured output session",
	})
	result, fallbackErr := a.runOnceWithFormat(ctx, opts, false)
	if fallbackErr != nil {
		return nil, fmt.Errorf("opencode prompt-only structured output fallback: %w", fallbackErr)
	}
	return result, nil
}

func (a *opencodeAgent) runOnceWithFormat(ctx context.Context, opts RunOpts, nativeFormat bool) (*Result, error) {
	// Start server on first invocation (synchronized)
	baseURL, err := a.ensureServer(ctx, opts.CWD, opts.Env)
	if err != nil {
		return nil, err
	}

	// Create session with blanket permissions
	sessionID, err := a.createSession(ctx, baseURL, opts.CWD)
	if err != nil {
		return nil, err
	}
	defer a.deleteSession(baseURL, sessionID)

	// Build prompt with schema instructions if provided
	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt = buildOpencodePrompt(prompt, opts.JSONSchema)
	}

	// Connect to SSE event stream
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	eventBody, err := a.connectEventStream(streamCtx, baseURL)
	if err != nil {
		return nil, err
	}
	defer eventBody.Close()

	// Send message concurrently — blocks until agent completes
	msgCtx, msgCancel := context.WithCancel(ctx)
	defer msgCancel()
	msgCh := make(chan opencodeMessageResult, 1)
	go func() {
		schema := opts.JSONSchema
		if !nativeFormat {
			schema = nil
		}
		resp, err := a.sendMessage(msgCtx, baseURL, sessionID, prompt, schema)
		msgCh <- opencodeMessageResult{resp: resp, err: err, settled: true}
	}()

	// Process SSE events until session.idle
	state := &opencodeStreamState{
		sessionID:  sessionID,
		onChunk:    opts.OnChunk,
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	err = parseOpencodeSSE(eventBody, state)
	streamCancel()

	if err != nil {
		// The stream carried the tool events, so with it gone the message
		// response is the remaining record of what the turn ran. Taking only
		// what has already arrived answers "nothing yet" while the request is
		// still in flight, and that is not the same answer as "no tool ran" -
		// so the evidence is resolved here, before anything classifies the
		// failure.
		mr := pollOpencodeMessage(msgCh)
		aborted := false
		if !mr.settled {
			// Aborting is the cleanup this branch already did. Doing it
			// first also ends the turn opencode is still running, which is
			// what makes the in-flight request answer - with the assistant
			// message and its parts. The wait is bounded: a server that is
			// gone never answers, and that turn is simply unverifiable.
			a.abortSession(baseURL, sessionID)
			aborted = true
			mr = awaitOpencodeMessage(msgCh, opencodeEvidenceWait)
		}
		evidence := resolveOpencodeToolEvidence(state, mr, false)
		if mr.settled && mr.err != nil {
			if nativeFormat && isThinkingToolChoiceConflictText(mr.err.Error()) {
				return nil, thinkingConflict(evidence, mr.err)
			}
			return nil, opencodeTurnFailure(evidence, fmt.Errorf("opencode message: %w", mr.err))
		}
		if !aborted {
			a.abortSession(baseURL, sessionID)
		}
		if nativeFormat && errors.Is(err, errOpencodeThinkingToolChoiceConflict) {
			return nil, thinkingConflict(evidence, nil)
		}
		return nil, opencodeTurnFailure(evidence, fmt.Errorf("opencode events: %w", err))
	}

	// Wait for message response. The stream ran to session.idle, so every
	// tool part of the session crossed it and the evidence is settled however
	// this request ends.
	mr := <-msgCh
	evidence := resolveOpencodeToolEvidence(state, mr, true)
	if mr.err != nil {
		if nativeFormat && isThinkingToolChoiceConflictText(mr.err.Error()) {
			return nil, thinkingConflict(evidence, mr.err)
		}
		return nil, opencodeTurnFailure(evidence, fmt.Errorf("opencode message: %w", mr.err))
	}

	// Update usage and text from message response
	responseText := ""
	responseFinalText := ""
	if mr.resp != nil && mr.resp.Info != nil {
		streamedText := state.lastText
		streamedFinalText := state.lastFinalText
		emitResponseChunk := func(chunk string) {
			if opts.OnChunk == nil || chunk == "" {
				return
			}
			state.emitSeparatorIfNeeded()
			opts.OnChunk(chunk)
			state.hasEmittedText = true
		}
		if mr.resp.Info.Role == "assistant" && mr.resp.Info.Tokens != nil {
			state.usageByMsg[mr.resp.Info.ID] = opencodeTokensToUsage(mr.resp.Info.Tokens)
			state.usage = accumulateUsage(state.usageByMsg)
		}
		for _, part := range mr.resp.Parts {
			if part.Type != "text" || strings.TrimSpace(part.Text) == "" {
				continue
			}
			responseText += part.Text
			if part.Metadata != nil && part.Metadata.OpenAI != nil && part.Metadata.OpenAI.Phase == "final_answer" {
				responseFinalText += part.Text
			}
		}
		if responseText != "" {
			state.lastText = responseText
		}
		if responseFinalText != "" {
			state.lastFinalText = responseFinalText
		}
		if responseFinalText != "" {
			responseText = responseFinalText
		}
		if opts.OnChunk != nil && responseText != "" {
			streamedResponseText := streamedText
			if streamedFinalText != "" {
				streamedResponseText = streamedFinalText
			}
			switch {
			case !state.hasEmittedText:
				emitResponseChunk(responseText)
			case streamedResponseText == "":
				emitResponseChunk(responseText)
			case strings.HasPrefix(responseText, streamedResponseText):
				suffix := responseText[len(streamedResponseText):]
				emitResponseChunk(suffix)
			}
		}
	}

	// Prefer structured output from response
	if mr.resp != nil && mr.resp.Info != nil && mr.resp.Info.Structured != nil {
		return &Result{
			Output:                mr.resp.Info.Structured,
			Text:                  state.lastText,
			Usage:                 state.usage,
			UsageReported:         state.usage.Reported,
			CacheCreationReported: state.usage.CacheCreationReported,
		}, nil
	}

	// A thinking model rejecting the forced tool_choice is handled by the
	// prompt-only fallback in runOnce, so it must be recognised before the
	// general failure below claims it.
	if nativeFormat && mr.resp != nil && mr.resp.Info != nil && isThinkingToolChoiceConflict(mr.resp.Info.Error) {
		return nil, thinkingConflict(evidence, nil)
	}

	// A turn that failed reports its cause on info.error rather than on the
	// HTTP status, so the request itself looks successful. Surface that error
	// instead of falling through to the streamed text: opencode leaves no
	// usable text behind a failed turn, so the fallback reports the
	// undiagnosable "opencode returned no text output" and hides causes such
	// as a provider rejecting the forced tool_choice that json_schema output
	// requires, or an expired provider credential. Any prose streamed before
	// the failure is reasoning, not an answer. This supersedes the narrower
	// StructuredOutputError-only branch: opencodeMessageFailure renders that
	// case with the same wording and decodes the nested error payload the
	// flat fields never carried.
	if mr.resp != nil && mr.resp.Info != nil && mr.resp.Info.Error != nil {
		return nil, newOpencodeMessageFailure(mr.resp.Info.Error, evidence == opencodeToolsRan)
	}

	// Fall back to parsing JSON from text
	outputText := state.lastFinalText
	if outputText == "" {
		outputText = state.lastText
	}
	result, err := finalizeTextResult("opencode", outputText, opts.JSONSchema, state.usage)
	if err != nil {
		// A parse failure quotes the model's own output, so whether it looks
		// transient to the shared classifier is decided by text the model
		// wrote. It takes the same gate as the rest.
		return nil, opencodeTurnFailure(evidence, err)
	}
	return result, nil
}

func isThinkingToolChoiceConflict(e *opencodeMessageError) bool {
	for _, text := range e.providerText() {
		if isThinkingToolChoiceConflictText(text) {
			return true
		}
	}
	return false
}

func isThinkingToolChoiceConflictText(text string) bool {
	for _, pattern := range thinkingToolChoiceConflictPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

func (a *opencodeAgent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		a.server.shutdown()
		a.server = nil
	}
	return nil
}
