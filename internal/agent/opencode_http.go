package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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
	args := buildOpencodeServeArgs(a.extraArgs, port)
	srv, err := startServerWithPort(ctx, "opencode", a.bin, args, cwd, "/global/health", port)
	if err != nil {
		return "", fmt.Errorf("opencode server: %w", err)
	}
	a.server = srv
	return srv.baseURL(), nil
}

// buildOpencodeServeArgs builds `opencode serve`'s argv with user-supplied
// server extras inserted after the "serve" subcommand and before the managed
// flags. Top-level run/tui flags such as --model are handled on the message
// request instead of being forwarded to `serve`, which does not accept them.
func buildOpencodeServeArgs(extraArgs []string, port int) []string {
	serverArgs := stripOpencodeModelArgs(extraArgs)
	args := make([]string, 0, len(serverArgs)+6)
	args = append(args, "serve")
	args = append(args, serverArgs...)
	args = append(args, "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--print-logs")
	return args
}

type opencodeModelOverride struct {
	providerID string
	modelID    string
}

// stripOpencodeModelArgs drops --model/-m (and their values) from extraArgs so
// `opencode serve` never receives run/tui-only flags it does not understand.
func stripOpencodeModelArgs(extraArgs []string) []string {
	if len(extraArgs) == 0 {
		return nil
	}
	filtered := make([]string, 0, len(extraArgs))
	for i := 0; i < len(extraArgs); i++ {
		arg := extraArgs[i]
		switch {
		case arg == "--model" || arg == "-m":
			if i+1 < len(extraArgs) {
				i++
			}
			continue
		case strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "-m="):
			continue
		default:
			filtered = append(filtered, arg)
		}
	}
	return filtered
}

// opencodeModelOverrideFromArgs parses the first --model/-m (in long, equals,
// or short form) into a provider/model override. It returns nil when no model
// flag is present so callers can preserve the default behavior.
func opencodeModelOverrideFromArgs(extraArgs []string) (*opencodeModelOverride, error) {
	var model string
	for i := 0; i < len(extraArgs); i++ {
		arg := extraArgs[i]
		switch {
		case arg == "--model" || arg == "-m":
			if i+1 >= len(extraArgs) {
				return nil, fmt.Errorf("opencode %s requires a provider/model value", arg)
			}
			i++
			model = strings.TrimSpace(extraArgs[i])
		case strings.HasPrefix(arg, "--model="):
			model = strings.TrimSpace(strings.TrimPrefix(arg, "--model="))
		case strings.HasPrefix(arg, "-m="):
			model = strings.TrimSpace(strings.TrimPrefix(arg, "-m="))
		}
	}
	if model == "" {
		return nil, nil
	}
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return nil, fmt.Errorf("opencode --model must be provider/model, got %q", model)
	}
	return &opencodeModelOverride{
		providerID: strings.TrimSpace(parts[0]),
		modelID:    strings.TrimSpace(parts[1]),
	}, nil
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
	model, err := opencodeModelOverrideFromArgs(a.extraArgs)
	if err != nil {
		return nil, err
	}
	if model != nil {
		body["model"] = map[string]string{
			"providerID": model.providerID,
			"modelID":    model.modelID,
		}
	}
	if len(schema) > 0 {
		body["format"] = map[string]any{
			"type":       "json_schema",
			"schema":     json.RawMessage(schema),
			"retryCount": 2,
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
