package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func (a *opencodeAgent) ensureServer(ctx context.Context, cwd string, env []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return a.server.baseURL(), nil
	}
	v2, err := a.detectV2(ctx, cwd, env)
	if err != nil {
		return "", err
	}
	port, err := getAvailablePort()
	if err != nil {
		return "", fmt.Errorf("opencode port: %w", err)
	}
	args := buildOpencodeServeArgs(a.extraArgs, port)
	health := "/global/health"
	options := managedServerOptions{}
	var v2Client opencodeV2Client
	if v2 {
		health = "/api/info"
		password := rand.Text()
		v2Client.authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+password))
		options.healthHeaders = map[string]string{"Authorization": v2Client.authorization}
		options.holdStdin = true
		args = append(args, "--stdio")
		// V2's stdio mode removes these lease credentials before tools inherit
		// the environment, and shuts down when our parent-owned pipe closes.
		env = append(append([]string(nil), env...), "OPENCODE_PASSWORD="+password, "OPENCODE_SERVER_PASSWORD="+password)
	}
	srv, err := startServerWithOptions(ctx, "opencode", a.bin, args, cwd, health, port, a.overlay(), options, env)
	if err != nil {
		return "", fmt.Errorf("opencode server: %w", err)
	}
	if v2 {
		var info struct {
			Version string `json:"version"`
		}
		err := v2Client.json(ctx, http.MethodGet, srv.baseURL()+"/api/info", nil, &info)
		if err != nil || !strings.HasPrefix(info.Version, "2.") {
			srv.shutdown()
			return "", fmt.Errorf("opencode server did not return valid v2 API metadata")
		}
	}
	a.v2 = v2
	a.v2Auth = v2Client.authorization
	a.server = srv
	return srv.baseURL(), nil
}

var opencodeVersionPattern = regexp.MustCompile(`^(?:opencode\s+)?v?([12])\.[0-9]+\.[0-9]+(?:[-+][a-zA-Z0-9.+-]+)?$`)

// Select a protocol before creating any session. Never discover a major-version
// mismatch by retrying an admitted prompt against a different API.
func (a *opencodeAgent) detectV2(ctx context.Context, cwd string, env []string) (bool, error) {
	probe, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probe, a.bin, "--version")
	cmd.Dir = cwd
	cmd.Env = a.gitSafeEnv(cwd, env)
	shellenv.ConfigureShellCommand(cmd)
	output, err := shellenv.OutputShellCommand(cmd)
	if err != nil {
		return false, fmt.Errorf("opencode version probe: %w", err)
	}
	match := opencodeVersionPattern.FindStringSubmatch(strings.TrimSpace(string(output)))
	if match == nil {
		return false, fmt.Errorf("opencode version probe: unsupported version (expected OpenCode 1.x or 2.x)")
	}
	return match[1] == "2", nil
}

// buildOpencodeServeArgs builds `opencode serve`'s argv with user-supplied
// extras inserted after the "serve" subcommand and before the managed flags.
func buildOpencodeServeArgs(extraArgs []string, port int) []string {
	args := make([]string, 0, len(extraArgs)+6)
	args = append(args, "serve")
	args = append(args, extraArgs...)
	args = append(args, "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--print-logs")
	return args
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
	respBytes, err := doJSON(ctx, http.MethodPost, baseURL+"/session/"+sessionID+"/message", nil, a.messageBody(prompt, schema))
	if err != nil {
		return nil, err
	}

	var resp opencodeMessageResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("opencode message parse: %w", err)
	}
	return &resp, nil
}

// messageBody builds the POST /session/{id}/message payload.
//
// Model and reasoning effort ride the body rather than argv because `opencode
// serve` has no flag for either: it exits with usage on an unknown option, so an
// argv-based pin would take the whole server down instead of tuning it. The
// provider/model split is validated at construction by agentcfg, so a non-empty
// model always yields the two fields opencode requires; opencode calls reasoning
// effort a provider-specific "variant".
func (a *opencodeAgent) messageBody(prompt string, schema json.RawMessage) map[string]any {
	body := map[string]any{
		"role":  "user",
		"parts": []map[string]string{{"type": "text", "text": prompt}},
	}
	if provider, modelID, ok := agentcfg.SplitProviderModel(a.profile.Model); ok {
		body["model"] = map[string]string{"providerID": provider, "modelID": modelID}
	}
	if a.profile.Effort != "" {
		body["variant"] = string(a.profile.Effort)
	}
	if len(schema) > 0 {
		body["info"] = map[string]any{
			"format": map[string]any{
				"type":       "json_schema",
				"schema":     json.RawMessage(schema),
				"retryCount": 2,
			},
		}
	}
	return body
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
