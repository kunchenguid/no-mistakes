package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// rovodevAgent starts a persistent HTTP server via `acli rovodev serve`
// and sends requests via REST with SSE streaming.
type rovodevAgent struct {
	bin    string
	mu     sync.Mutex
	server *managedServer
}

func (a *rovodevAgent) Name() string { return "rovodev" }

func (a *rovodevAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	// Start server on first invocation (synchronized)
	baseURL, err := a.ensureServer(ctx, opts.CWD)
	if err != nil {
		return nil, err
	}

	// Create session
	sessionID, err := a.createSession(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	defer a.deleteSession(baseURL, sessionID)

	// Set system prompt if schema provided
	if len(opts.JSONSchema) > 0 {
		prompt := buildRovodevSystemPrompt(opts.JSONSchema)
		if err := a.setSystemPrompt(ctx, baseURL, sessionID, prompt); err != nil {
			return nil, err
		}
	}

	// Send chat message
	if err := a.setChatMessage(ctx, baseURL, sessionID, opts.Prompt); err != nil {
		return nil, err
	}

	// Stream chat response
	var usage TokenUsage
	text, err := a.streamChat(ctx, baseURL, sessionID, opts.OnChunk, &usage)
	if err != nil {
		// Best-effort cancel on error
		a.cancelSession(baseURL, sessionID)
		return nil, err
	}

	return finalizeTextResult("rovodev", text, opts.JSONSchema, usage)
}

func (a *rovodevAgent) ensureServer(ctx context.Context, cwd string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return a.server.baseURL(), nil
	}
	port, err := getAvailablePort()
	if err != nil {
		return "", fmt.Errorf("rovodev port: %w", err)
	}
	args := []string{"rovodev", "serve", "--disable-session-token", fmt.Sprintf("%d", port)}
	srv, err := startServerWithPort(ctx, "rovodev", a.bin, args, cwd, "/healthcheck", port)
	if err != nil {
		return "", fmt.Errorf("rovodev server: %w", err)
	}
	a.server = srv
	return srv.baseURL(), nil
}

func (a *rovodevAgent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		a.server.shutdown()
		a.server = nil
	}
	return nil
}

func (a *rovodevAgent) createSession(ctx context.Context, baseURL string) (string, error) {
	body := map[string]string{"custom_title": "no-mistakes"}
	resp, err := doJSON(ctx, http.MethodPost, baseURL+"/v3/sessions/create", nil, body)
	if err != nil {
		return "", fmt.Errorf("rovodev create session: %w", err)
	}

	var result struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("rovodev create session parse: %w", err)
	}
	return result.SessionID, nil
}

func (a *rovodevAgent) setSystemPrompt(ctx context.Context, baseURL, sessionID, prompt string) error {
	body := map[string]string{"prompt": prompt}
	headers := map[string]string{"x-session-id": sessionID}
	_, err := doJSON(ctx, http.MethodPut, baseURL+"/v3/inline-system-prompt", headers, body)
	if err != nil {
		return fmt.Errorf("rovodev set system prompt: %w", err)
	}
	return nil
}

func (a *rovodevAgent) setChatMessage(ctx context.Context, baseURL, sessionID, message string) error {
	body := map[string]string{"message": message}
	headers := map[string]string{"x-session-id": sessionID}
	_, err := doJSON(ctx, http.MethodPost, baseURL+"/v3/set_chat_message", headers, body)
	if err != nil {
		return fmt.Errorf("rovodev set chat message: %w", err)
	}
	return nil
}

func (a *rovodevAgent) streamChat(ctx context.Context, baseURL, sessionID string, onChunk func(string), usage *TokenUsage) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v3/stream_chat", nil)
	if err != nil {
		return "", fmt.Errorf("rovodev stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-session-id", sessionID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("rovodev stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("rovodev stream failed with %d: %s", resp.StatusCode, string(body))
	}

	var latestText string
	err = parseRovodevSSE(resp.Body, onChunk, usage, &latestText)
	return latestText, err
}

func (a *rovodevAgent) cancelSession(baseURL, sessionID string) {
	headers := map[string]string{"x-session-id": sessionID}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	doJSON(ctx, http.MethodPost, baseURL+"/v3/cancel", headers, nil)
}

func (a *rovodevAgent) deleteSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/v3/sessions/"+sessionID, nil)
	if req != nil {
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp != nil {
			resp.Body.Close()
		}
	}
}

// buildRovodevSystemPrompt creates a system prompt that instructs the agent
// to return structured JSON matching the given schema.
func buildRovodevSystemPrompt(schema json.RawMessage) string {
	return strings.Join([]string{
		"When you finish, reply with only valid JSON.",
		"Do not wrap the JSON in markdown fences.",
		"Do not include any prose before or after the JSON.",
		"The JSON must match this schema exactly: " + string(schema),
	}, "\n")
}

// rovodevSSEEvent captures every field we care about from a rovodev SSE
// data payload. Real rovodev emits text content via part_start / part_delta
// events (not a single "text" event) and puts request-usage fields at the
// top level of the payload, not nested under "usage". Shape matches the
// well-proven gnhf integration.
type rovodevSSEEvent struct {
	EventKind string `json:"event_kind,omitempty"`
	Content   string `json:"content,omitempty"`

	// request-usage
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`

	// part_start
	Index int             `json:"index,omitempty"`
	Part  *rovodevSSEPart `json:"part,omitempty"`

	// part_delta
	Delta *rovodevSSEDelta `json:"delta,omitempty"`
}

type rovodevSSEPart struct {
	Content  string `json:"content"`
	PartKind string `json:"part_kind"`
}

type rovodevSSEDelta struct {
	ContentDelta  string `json:"content_delta"`
	PartDeltaKind string `json:"part_delta_kind"`
}

// parseRovodevSSE processes the SSE stream from rovodev, extracting text
// chunks, token usage, and the latest assembled text for structured output.
//
// Text arrives in three possible shapes:
//   - "text" events carry the full current message in `content`.
//   - "part_start" events start a new text part at an index.
//   - "part_delta" events append to the part at the given index.
//
// Tool activity ("tool-return", "on_call_tools_start") resets the buffer so
// only the final post-tool text segment is returned as the structured answer.
func parseRovodevSSE(r io.Reader, onChunk func(string), usage *TokenUsage, latestText *string) error {
	var parts []string
	partIndex := map[int]int{}
	var hasEmittedText, hadToolActivity bool

	updateLatest := func() {
		*latestText = strings.Join(parts, "")
	}
	resetParts := func() {
		parts = nil
		partIndex = map[int]int{}
		*latestText = ""
	}
	emitSeparator := func() {
		if hasEmittedText && hadToolActivity && onChunk != nil {
			onChunk("\n\n")
		}
		hadToolActivity = false
	}
	emitChunk := func(s string) {
		if onChunk != nil {
			onChunk(s)
			hasEmittedText = true
		}
	}

	return parseSSE(r, func(ev sseEvent) bool {
		if ev.Data == "" {
			return true
		}

		kind := ev.Name
		var payload rovodevSSEEvent
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			return true
		}
		if kind == "" && payload.EventKind != "" {
			kind = payload.EventKind
		}

		switch kind {
		case "request-usage":
			usage.Add(TokenUsage{
				InputTokens:         payload.InputTokens,
				OutputTokens:        payload.OutputTokens,
				CacheReadTokens:     payload.CacheReadTokens,
				CacheCreationTokens: payload.CacheWriteTokens,
			})

		case "text":
			if payload.Content != "" {
				emitSeparator()
				parts = []string{payload.Content}
				partIndex = map[int]int{0: 0}
				updateLatest()
				emitChunk(payload.Content)
			}

		case "part_start":
			if payload.Part != nil && payload.Part.PartKind == "text" && payload.Part.Content != "" {
				emitSeparator()
				parts = append(parts, payload.Part.Content)
				partIndex[payload.Index] = len(parts) - 1
				updateLatest()
				emitChunk(payload.Part.Content)
			}

		case "part_delta":
			if payload.Delta != nil && payload.Delta.PartDeltaKind == "text" && payload.Delta.ContentDelta != "" {
				if idx, ok := partIndex[payload.Index]; ok {
					parts[idx] += payload.Delta.ContentDelta
				} else {
					emitSeparator()
					parts = append(parts, payload.Delta.ContentDelta)
					partIndex[payload.Index] = len(parts) - 1
				}
				updateLatest()
				emitChunk(payload.Delta.ContentDelta)
			}

		case "tool-return", "on_call_tools_start":
			resetParts()
			hadToolActivity = true
		}

		return true
	})
}

// doJSON makes an HTTP request with JSON body and returns the response body.
func doJSON(ctx context.Context, method, url string, headers map[string]string, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s failed with %d: %s", method, url, resp.StatusCode, string(respBody))
	}

	return respBody, nil
}
