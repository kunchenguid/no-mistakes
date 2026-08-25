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

// thinkingConflict builds the fallback trigger, recording whether the turn
// had already invoked a tool. A session.error can arrive at any point in a
// turn, so the conflict is not always detected before the model has acted.
func thinkingConflict(state *opencodeStreamState, resp *opencodeMessageResponse, cause error) error {
	err := errOpencodeThinkingToolChoiceConflict
	if opencodeTurnRanTools(state, resp) {
		err = fmt.Errorf("%w (%w)", err, errOpencodeToolsAlreadyRan)
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
	// already invoked a tool would replay its side effects. Same reasoning as
	// classifyOpencodeTransient, and the same fail-closed answer: report the
	// conflict and let the operator decide.
	if errors.Is(err, errOpencodeToolsAlreadyRan) {
		return nil, err
	}

	// OpenCode implements json_schema output as a required StructuredOutput
	// tool call. Some thinking-enabled models reject that combination. Retry
	// once without the native format, while keeping the schema in the prompt
	// and validating the returned JSON against it in finalizeTextResult.
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
	type messageResult struct {
		resp *opencodeMessageResponse
		err  error
	}
	msgCtx, msgCancel := context.WithCancel(ctx)
	defer msgCancel()
	msgCh := make(chan messageResult, 1)
	go func() {
		schema := opts.JSONSchema
		if !nativeFormat {
			schema = nil
		}
		resp, err := a.sendMessage(msgCtx, baseURL, sessionID, prompt, schema)
		msgCh <- messageResult{resp: resp, err: err}
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
		// Check if message request failed
		select {
		case mr := <-msgCh:
			if mr.err != nil {
				if nativeFormat && isThinkingToolChoiceConflictText(mr.err.Error()) {
					return nil, thinkingConflict(state, nil, mr.err)
				}
				return nil, fmt.Errorf("opencode message: %w", mr.err)
			}
		default:
		}
		a.abortSession(baseURL, sessionID)
		if nativeFormat && errors.Is(err, errOpencodeThinkingToolChoiceConflict) {
			return nil, thinkingConflict(state, nil, nil)
		}
		return nil, fmt.Errorf("opencode events: %w", err)
	}

	// Wait for message response
	mr := <-msgCh
	if mr.err != nil {
		if nativeFormat && isThinkingToolChoiceConflictText(mr.err.Error()) {
			return nil, thinkingConflict(state, nil, mr.err)
		}
		return nil, fmt.Errorf("opencode message: %w", mr.err)
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
		return nil, thinkingConflict(state, mr.resp, nil)
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
		return nil, newOpencodeMessageFailure(mr.resp.Info.Error, opencodeTurnRanTools(state, mr.resp))
	}

	// Fall back to parsing JSON from text
	outputText := state.lastFinalText
	if outputText == "" {
		outputText = state.lastText
	}
	return finalizeTextResult("opencode", outputText, opts.JSONSchema, state.usage)
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
