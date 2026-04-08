package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// rovodevAgent starts a persistent HTTP server via `acli rovodev serve`
// and sends requests via REST with SSE streaming.
type rovodevAgent struct {
	bin    string
	server *managedServer
}

func (a *rovodevAgent) Name() string { return "rovodev" }

func (a *rovodevAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	// Start server on first invocation
	if a.server == nil {
		port, err := getAvailablePort()
		if err != nil {
			return nil, fmt.Errorf("rovodev port: %w", err)
		}
		args := []string{"rovodev", "serve", "--disable-session-token", fmt.Sprintf("%d", port)}
		srv, err := startServerWithPort(ctx, a.bin, args, opts.CWD, "/healthcheck", port)
		if err != nil {
			return nil, fmt.Errorf("rovodev server: %w", err)
		}
		a.server = srv
	}

	baseURL := a.server.baseURL()

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

func (a *rovodevAgent) Close() error {
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
		http.DefaultClient.Do(req)
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

// rovodevSSEEvent is the JSON payload from a rovodev SSE data field.
type rovodevSSEEvent struct {
	EventKind string           `json:"event_kind,omitempty"`
	Content   string           `json:"content,omitempty"`
	Usage     *rovodevSSEUsage `json:"usage,omitempty"`
}

type rovodevSSEUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// parseRovodevSSE processes the SSE stream from rovodev, extracting text
// chunks, token usage, and the latest text segment for structured output.
func parseRovodevSSE(r io.Reader, onChunk func(string), usage *TokenUsage, latestText *string) error {
	return parseSSE(r, func(ev sseEvent) bool {
		if ev.Data == "" {
			return true
		}

		// Determine event kind from SSE event name or JSON payload
		kind := ev.Name

		var payload rovodevSSEEvent
		if err := json.Unmarshal([]byte(ev.Data), &payload); err == nil {
			if kind == "" && payload.EventKind != "" {
				kind = payload.EventKind
			}

			switch kind {
			case "request-usage":
				if payload.Usage != nil {
					usage.Add(TokenUsage{
						InputTokens:         payload.Usage.InputTokens,
						OutputTokens:        payload.Usage.OutputTokens,
						CacheReadTokens:     payload.Usage.CacheReadTokens,
						CacheCreationTokens: payload.Usage.CacheWriteTokens,
					})
				}

			case "text":
				if payload.Content != "" {
					*latestText = payload.Content
					if onChunk != nil {
						onChunk(payload.Content)
					}
				}

			case "tool-return", "on_call_tools_start":
				// Reset text buffer — agent is doing tool calls
				*latestText = ""
			}
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
