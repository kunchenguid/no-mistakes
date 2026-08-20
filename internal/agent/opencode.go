package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

var errOpencodeThinkingToolChoiceConflict = errors.New("opencode provider rejects required tool choice while thinking is enabled")

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
	mu        sync.Mutex
	server    *managedServer
}

func (a *opencodeAgent) Name() string { return "opencode" }

func (a *opencodeAgent) ReportsAgentAttempts() bool { return true }

func (a *opencodeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "opencode", opts, claudeMaxRetries, classifyTransient, a.recoverTransientRetry, func() (*Result, error) {
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
					return nil, fmt.Errorf("%w: %v", errOpencodeThinkingToolChoiceConflict, mr.err)
				}
				return nil, fmt.Errorf("opencode message: %w", mr.err)
			}
		default:
		}
		a.abortSession(baseURL, sessionID)
		return nil, fmt.Errorf("opencode events: %w", err)
	}

	// Wait for message response
	mr := <-msgCh
	if mr.err != nil {
		if nativeFormat && isThinkingToolChoiceConflictText(mr.err.Error()) {
			return nil, fmt.Errorf("%w: %v", errOpencodeThinkingToolChoiceConflict, mr.err)
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

	if nativeFormat && mr.resp != nil && mr.resp.Info != nil && isThinkingToolChoiceConflict(mr.resp.Info.Error) {
		return nil, errOpencodeThinkingToolChoiceConflict
	}

	// Surface opencode's StructuredOutputError directly. When the model
	// fails to call the StructuredOutput tool after the configured retries,
	// opencode sets info.error.name = "StructuredOutputError" and the
	// streamed text is just reasoning prose - feeding it to
	// finalizeTextResult produces the misleading "invalid character 'N'
	// looking for beginning of value" error.
	if mr.resp != nil && mr.resp.Info != nil && mr.resp.Info.Error.IsStructuredOutput() {
		retries := 0
		if mr.resp.Info.Error.Retries != nil {
			retries = *mr.resp.Info.Error.Retries
		}
		return nil, fmt.Errorf("opencode structured output failed after %d internal retries: %s",
			retries, mr.resp.Info.Error.Message)
	}

	// Fall back to parsing JSON from text
	outputText := state.lastFinalText
	if outputText == "" {
		outputText = state.lastText
	}
	return finalizeTextResult("opencode", outputText, opts.JSONSchema, state.usage)
}

func isThinkingToolChoiceConflict(e *opencodeMessageError) bool {
	if e == nil {
		return false
	}
	raw, err := json.Marshal(e)
	return err == nil && isThinkingToolChoiceConflictText(string(raw))
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
