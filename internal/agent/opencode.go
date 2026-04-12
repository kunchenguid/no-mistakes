package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// opencodeAgent starts a persistent HTTP server via `opencode serve`
// and sends requests via REST with SSE streaming.
type opencodeAgent struct {
	bin    string
	mu     sync.Mutex
	server *managedServer
}

func (a *opencodeAgent) Name() string { return "opencode" }

func (a *opencodeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	// Start server on first invocation (synchronized)
	baseURL, err := a.ensureServer(ctx, opts.CWD)
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
		resp, err := a.sendMessage(msgCtx, baseURL, sessionID, prompt, opts.JSONSchema)
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
		return nil, fmt.Errorf("opencode message: %w", mr.err)
	}

	// Update usage and text from message response
	if mr.resp != nil && mr.resp.Info != nil {
		if mr.resp.Info.Role == "assistant" && mr.resp.Info.Tokens != nil {
			state.usageByMsg[mr.resp.Info.ID] = opencodeTokensToUsage(mr.resp.Info.Tokens)
			state.usage = accumulateUsage(state.usageByMsg)
		}
		for _, part := range mr.resp.Parts {
			if part.Type != "text" || strings.TrimSpace(part.Text) == "" {
				continue
			}
			state.lastText = part.Text
			if part.Metadata != nil && part.Metadata.OpenAI != nil && part.Metadata.OpenAI.Phase == "final_answer" {
				state.lastFinalText = part.Text
			}
		}
	}

	// Prefer structured output from response
	if mr.resp != nil && mr.resp.Info != nil && mr.resp.Info.Structured != nil {
		return &Result{
			Output: mr.resp.Info.Structured,
			Text:   state.lastText,
			Usage:  state.usage,
		}, nil
	}

	// Fall back to parsing JSON from text
	outputText := state.lastFinalText
	if outputText == "" {
		outputText = state.lastText
	}
	return finalizeTextResult("opencode", outputText, opts.JSONSchema, state.usage)
}

func (a *opencodeAgent) ensureServer(ctx context.Context, cwd string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return a.server.baseURL(), nil
	}
	port, err := getAvailablePort()
	if err != nil {
		return "", fmt.Errorf("opencode port: %w", err)
	}
	args := []string{"serve", "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--print-logs"}
	srv, err := startServerWithPort(ctx, a.bin, args, cwd, "/global/health", port)
	if err != nil {
		return "", fmt.Errorf("opencode server: %w", err)
	}
	a.server = srv
	return srv.baseURL(), nil
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

func (a *opencodeAgent) createSession(ctx context.Context, baseURL, cwd string) (string, error) {
	body := map[string]any{
		"directory": cwd,
		"permission": []map[string]string{
			{"permission": "*", "pattern": "*", "action": "allow"},
		},
	}
	resp, err := doJSON(ctx, http.MethodPost, baseURL+"/session", nil, body)
	if err != nil {
		return "", fmt.Errorf("opencode create session: %w", err)
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("opencode create session parse: %w", err)
	}
	return result.ID, nil
}

func (a *opencodeAgent) connectEventStream(ctx context.Context, baseURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/global/event", nil)
	if err != nil {
		return nil, fmt.Errorf("opencode event stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode event stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("opencode event stream failed with %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

func (a *opencodeAgent) sendMessage(ctx context.Context, baseURL, sessionID, prompt string, schema json.RawMessage) (*opencodeMessageResponse, error) {
	body := map[string]any{
		"role":  "user",
		"parts": []map[string]string{{"type": "text", "text": prompt}},
	}
	if len(schema) > 0 {
		body["format"] = map[string]any{
			"type":       "json_schema",
			"schema":     json.RawMessage(schema),
			"retryCount": 1,
		}
	}

	respBytes, err := doJSON(ctx, http.MethodPost, baseURL+"/session/"+sessionID+"/message", nil, body)
	if err != nil {
		return nil, err
	}

	var resp opencodeMessageResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("opencode message parse: %w", err)
	}
	return &resp, nil
}

func (a *opencodeAgent) abortSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	doJSON(ctx, http.MethodPost, baseURL+"/session/"+sessionID+"/abort", nil, nil)
}

func (a *opencodeAgent) deleteSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/session/"+sessionID, nil)
	if req != nil {
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp != nil {
			resp.Body.Close()
		}
	}
}

// buildOpencodePrompt appends schema instructions to the prompt.
func buildOpencodePrompt(prompt string, schema json.RawMessage) string {
	return strings.Join([]string{
		prompt,
		"",
		"When you finish, reply with only valid JSON.",
		"Do not wrap the JSON in markdown fences.",
		"Do not include any prose before or after the JSON.",
		"The JSON must match this schema exactly: " + string(schema),
	}, "\n")
}

// --- SSE event types ---

// opencodeStreamEvent is the top-level JSON from an OpenCode SSE data field.
type opencodeStreamEvent struct {
	Directory string                      `json:"directory,omitempty"`
	Payload   *opencodeStreamEventPayload `json:"payload,omitempty"`
}

type opencodeStreamEventPayload struct {
	Type       string                         `json:"type"`
	Properties *opencodeStreamEventProperties `json:"properties,omitempty"`
}

type opencodeStreamEventProperties struct {
	SessionID string             `json:"sessionID,omitempty"`
	Field     string             `json:"field,omitempty"`
	Delta     string             `json:"delta,omitempty"`
	PartID    string             `json:"partID,omitempty"`
	Part      *opencodeEventPart `json:"part,omitempty"`
	Info      *opencodeEventInfo `json:"info,omitempty"`
}

type opencodeEventPart struct {
	ID        string            `json:"id,omitempty"`
	MessageID string            `json:"messageID,omitempty"`
	Type      string            `json:"type,omitempty"`
	Text      string            `json:"text,omitempty"`
	Tokens    *opencodeTokens   `json:"tokens,omitempty"`
	Metadata  *opencodeMetadata `json:"metadata,omitempty"`
}

type opencodeEventInfo struct {
	ID     string          `json:"id,omitempty"`
	Role   string          `json:"role,omitempty"`
	Tokens *opencodeTokens `json:"tokens,omitempty"`
}

// opencodeTokens is the token usage structure in OpenCode responses.
type opencodeTokens struct {
	Input  int            `json:"input"`
	Output int            `json:"output"`
	Cache  *opencodeCache `json:"cache,omitempty"`
}

type opencodeCache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

type opencodeMetadata struct {
	OpenAI *opencodeOpenAI `json:"openai,omitempty"`
}

type opencodeOpenAI struct {
	Phase string `json:"phase,omitempty"`
}

// opencodeMessageResponse is the JSON body from POST /session/{id}/message.
type opencodeMessageResponse struct {
	Info  *opencodeMessageInfo  `json:"info,omitempty"`
	Parts []opencodeMessagePart `json:"parts,omitempty"`
}

type opencodeMessageInfo struct {
	ID         string          `json:"id,omitempty"`
	Role       string          `json:"role,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Tokens     *opencodeTokens `json:"tokens,omitempty"`
}

type opencodeMessagePart struct {
	Type     string            `json:"type,omitempty"`
	Text     string            `json:"text,omitempty"`
	Metadata *opencodeMetadata `json:"metadata,omitempty"`
}

// opencodeTextPart tracks accumulated text for a part ID during streaming.
type opencodeTextPart struct {
	text  string
	phase string
}

// opencodeStreamState holds mutable state during SSE event processing.
type opencodeStreamState struct {
	sessionID       string
	onChunk         func(string)
	textParts       map[string]*opencodeTextPart
	usageByMsg      map[string]TokenUsage
	usage           TokenUsage
	lastText        string
	lastFinalText   string
	userMsgIDs      map[string]bool
	hasEmittedText  bool
	hadToolActivity bool
}

func opencodeTokensToUsage(t *opencodeTokens) TokenUsage {
	u := TokenUsage{
		InputTokens:  t.Input,
		OutputTokens: t.Output,
	}
	if t.Cache != nil {
		u.CacheReadTokens = t.Cache.Read
		u.CacheCreationTokens = t.Cache.Write
	}
	return u
}

func accumulateUsage(byMsg map[string]TokenUsage) TokenUsage {
	var total TokenUsage
	for _, u := range byMsg {
		total.Add(u)
	}
	return total
}

// parseOpencodeSSE processes the SSE stream from OpenCode's /global/event endpoint.
func parseOpencodeSSE(r io.Reader, state *opencodeStreamState) error {
	var sawIdle bool
	err := parseSSE(r, func(ev sseEvent) bool {
		if ev.Data == "" {
			return true
		}

		var event opencodeStreamEvent
		if err := json.Unmarshal([]byte(ev.Data), &event); err != nil {
			return true // skip malformed events
		}

		payload := event.Payload
		if payload == nil {
			return true
		}
		props := payload.Properties

		// Filter by session ID
		if props != nil && props.SessionID != "" && props.SessionID != state.sessionID {
			return true
		}

		switch payload.Type {
		case "message.part.delta":
			if props != nil && props.Field == "text" && props.PartID != "" && props.Delta != "" {
				part := state.textParts[props.PartID]
				if part == nil {
					part = &opencodeTextPart{}
					state.textParts[props.PartID] = part
				}
				part.text += props.Delta
				state.updateText(part.text, part.phase)
				if state.onChunk != nil {
					state.emitSeparatorIfNeeded()
					state.onChunk(props.Delta)
					state.hasEmittedText = true
				}
			}

		case "message.part.updated":
			if props != nil && props.Part != nil {
				p := props.Part
				if p.Type == "text" && p.ID != "" {
					// Skip parts belonging to user messages
					if p.MessageID != "" && state.userMsgIDs[p.MessageID] {
						break
					}
					phase := ""
					if p.Metadata != nil && p.Metadata.OpenAI != nil {
						phase = p.Metadata.OpenAI.Phase
					}
					existing := state.textParts[p.ID]
					chunk := ""
					if existing != nil {
						if strings.HasPrefix(p.Text, existing.text) {
							chunk = p.Text[len(existing.text):]
						} else if p.Text != existing.text {
							chunk = p.Text
						}
						existing.text = p.Text
						existing.phase = phase
					} else {
						state.textParts[p.ID] = &opencodeTextPart{text: p.Text, phase: phase}
						chunk = p.Text
					}
					state.updateText(p.Text, phase)
					if state.onChunk != nil && chunk != "" {
						state.emitSeparatorIfNeeded()
						state.onChunk(chunk)
						state.hasEmittedText = true
					}
				}
				if p.Type == "step-finish" {
					state.hadToolActivity = true
					if p.MessageID != "" && p.Tokens != nil {
						state.usageByMsg[p.MessageID] = opencodeTokensToUsage(p.Tokens)
						state.usage = accumulateUsage(state.usageByMsg)
					}
				}
			}

		case "message.updated":
			if props != nil && props.Info != nil {
				if props.Info.Role == "user" {
					if state.userMsgIDs == nil {
						state.userMsgIDs = make(map[string]bool)
					}
					state.userMsgIDs[props.Info.ID] = true
				}
				if props.Info.Role == "assistant" && props.Info.Tokens != nil {
					state.usageByMsg[props.Info.ID] = opencodeTokensToUsage(props.Info.Tokens)
					state.usage = accumulateUsage(state.usageByMsg)
				}
			}

		case "session.idle":
			sawIdle = true
			return false
		}

		return true
	})

	if err != nil {
		return err
	}
	if !sawIdle {
		// Stream ended without session.idle — not an error if message response
		// will provide the final result
	}
	return nil
}

func (s *opencodeStreamState) emitSeparatorIfNeeded() {
	if s.hasEmittedText && s.hadToolActivity && s.onChunk != nil {
		s.onChunk("\n\n")
		s.hadToolActivity = false
	}
}

func (s *opencodeStreamState) updateText(text, phase string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	s.lastText = text
	if phase == "final_answer" {
		s.lastFinalText = text
	}
}
