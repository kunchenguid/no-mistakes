package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

// opencode 2.x guards every /api route with HTTP basic auth. The username is
// fixed to "opencode" by the server; the password is whatever the serve
// process was handed in its environment, or a random one it prints to stdout
// when none was. The adapter always supplies its own, so it never has to
// scrape the password out of the server's log stream, and the SPA fallback
// (any unknown GET answers 200 with the web UI's HTML) can never pass for a
// ready API: only an authenticated GET /api/info does.
const (
	opencodeAuthUser = "opencode"
	// opencodeServerPasswordEnv is the documented variable for a manually
	// run `opencode serve`.
	opencodeServerPasswordEnv = "OPENCODE_SERVER_PASSWORD"
	// opencodePasswordEnv is the variable opencode's CLI consults FIRST,
	// because it doubles as the client-side credential for `--server`. An
	// operator who has it set for some other server would otherwise have
	// it silently win over the password the daemon generated, so the
	// adapter sets both to the same value.
	opencodePasswordEnv = "OPENCODE_PASSWORD"
	opencodeInfoPath    = "/api/info"
)

func newOpencodeServerPassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("opencode server password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func opencodeAuthHeaders(password string) map[string]string {
	token := base64.StdEncoding.EncodeToString([]byte(opencodeAuthUser + ":" + password))
	return map[string]string{"Authorization": "Basic " + token}
}

// opencodeHealthProbe is the readiness check for a spawned `opencode serve`.
// It polls the authenticated /api/info route and fails FAST on the two
// statuses that a ready-but-wrong server answers with, instead of spending
// the whole health deadline on them: 404 means the process listens without
// the v2 API (an opencode 1.x binary, whose flat routes this adapter no
// longer speaks), and 401 means the server did not adopt the password it
// was handed (for example a `--service` instance keyed by its own config).
func opencodeHealthProbe(password string) healthProbe {
	return healthProbe{
		path:    opencodeInfoPath,
		headers: opencodeAuthHeaders(password),
		reject: func(status int) error {
			switch status {
			case http.StatusNotFound:
				return fmt.Errorf("opencode serve answered %s with 404: no v2 API; no-mistakes requires opencode 2.x", opencodeInfoPath)
			case http.StatusUnauthorized:
				return fmt.Errorf("opencode serve rejected the generated server password at %s (401); check that %s is not overridden by the serve command's configuration", opencodeInfoPath, opencodeServerPasswordEnv)
			}
			return nil
		},
	}
}

func (a *opencodeAgent) ensureServer(ctx context.Context, cwd string, env []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return a.server.baseURL(), nil
	}
	port, err := getAvailablePort()
	if err != nil {
		return "", fmt.Errorf("opencode port: %w", err)
	}
	password, err := newOpencodeServerPassword()
	if err != nil {
		return "", err
	}
	args := buildOpencodeServeArgs(a.extraArgs, port)
	serverEnv := make([]string, 0, len(env)+2)
	serverEnv = append(serverEnv, env...)
	serverEnv = append(serverEnv,
		opencodeServerPasswordEnv+"="+password,
		opencodePasswordEnv+"="+password,
	)
	srv, err := startServerWithPort(ctx, "opencode", a.bin, args, cwd, opencodeHealthProbe(password), port, a.overlay(), serverEnv)
	if err != nil {
		return "", fmt.Errorf("opencode server: %w", err)
	}
	a.server = srv
	a.password = password
	return srv.baseURL(), nil
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

// authHeaders is the basic-auth header for the running server. It reads the
// password under the same lock ensureServer writes it, because a transient
// retry can replace the server (and so the password) between two requests.
func (a *opencodeAgent) authHeaders() map[string]string {
	a.mu.Lock()
	password := a.password
	a.mu.Unlock()
	return opencodeAuthHeaders(password)
}

// createSession opens a fresh session rooted at cwd with every permission
// pre-granted, so no tool call ever parks the turn on a permission prompt
// nobody is there to answer. Model and reasoning effort travel here too:
// opencode 2.x selects them per SESSION (v1 took them per message), and
// `opencode serve` still has no flag for either, so argv is not an option
// (it exits with usage on an unknown option, taking the server down instead
// of tuning it). agentcfg validates the provider/model split at construction,
// so a non-empty model always yields the two fields opencode requires;
// opencode calls reasoning effort a provider-specific "variant".
func (a *opencodeAgent) createSession(ctx context.Context, baseURL, cwd string) (string, error) {
	resp, err := doJSON(ctx, http.MethodPost, baseURL+"/api/session", a.authHeaders(), a.sessionBody(cwd))
	if err != nil {
		return "", fmt.Errorf("opencode create session: %w", err)
	}

	var result opencodeSessionCreated
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("opencode create session parse: %w", err)
	}
	if result.Data.ID == "" {
		return "", fmt.Errorf("opencode create session: response carried no session id: %s", outputSnippet(string(resp)))
	}
	return result.Data.ID, nil
}

// sessionBody builds the POST /api/session payload: the session's location,
// its blanket permission ruleset, and the profile's model pin when there is
// one. Model.Ref names the model `id` (v1 said modelID) and carries the
// variant inside the ref.
func (a *opencodeAgent) sessionBody(cwd string) map[string]any {
	body := map[string]any{
		"location": map[string]string{"directory": cwd},
		"permissions": []map[string]string{
			{"action": "*", "resource": "*", "effect": "allow"},
		},
	}
	if provider, modelID, ok := agentcfg.SplitProviderModel(a.profile.Model); ok {
		model := map[string]string{"providerID": provider, "id": modelID}
		if a.profile.Effort != "" {
			model["variant"] = string(a.profile.Effort)
		}
		body["model"] = model
	}
	return body
}

func (a *opencodeAgent) connectEventStream(ctx context.Context, baseURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/event", nil)
	if err != nil {
		return nil, fmt.Errorf("opencode event stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range a.authHeaders() {
		req.Header.Set(k, v)
	}

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

// sendPrompt admits the prompt to the session. opencode 2.x answers as soon
// as the input is durably queued and the agent loop is scheduled; the turn
// itself is followed on the event stream. The payload is PromptInput: a
// `text` field, not v1's role/parts array.
func (a *opencodeAgent) sendPrompt(ctx context.Context, baseURL, sessionID, prompt string) error {
	body := map[string]any{"text": prompt}
	if _, err := doJSON(ctx, http.MethodPost, baseURL+"/api/session/"+sessionID+"/prompt", a.authHeaders(), body); err != nil {
		return err
	}
	return nil
}

// fetchMessages reads the session's message list, oldest first.
func (a *opencodeAgent) fetchMessages(ctx context.Context, baseURL, sessionID string) ([]opencodeMessage, error) {
	resp, err := doJSON(ctx, http.MethodGet, baseURL+"/api/session/"+sessionID+"/message", a.authHeaders(), nil)
	if err != nil {
		return nil, err
	}
	var list opencodeMessageList
	if err := json.Unmarshal(resp, &list); err != nil {
		return nil, fmt.Errorf("opencode messages parse: %w", err)
	}
	sortOpencodeMessages(list.Data)
	return list.Data, nil
}

// waitForIdle blocks until the session's agent loop is idle. It is bounded
// by ctx; the route is experimental upstream, so a failure here is only ever
// a missing shortcut and the caller re-reads the message list either way.
func (a *opencodeAgent) waitForIdle(ctx context.Context, baseURL, sessionID string) error {
	_, err := doJSON(ctx, http.MethodPost, baseURL+"/api/experimental/session/"+sessionID+"/wait", a.authHeaders(), nil)
	return err
}

// interruptSession stops an execution still running in the session. It is
// the v2 counterpart of v1's /abort.
func (a *opencodeAgent) interruptSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	doJSON(ctx, http.MethodPost, baseURL+"/api/session/"+sessionID+"/interrupt", a.authHeaders(), nil)
}

func (a *opencodeAgent) deleteSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/api/session/"+sessionID, nil)
	if err != nil {
		return
	}
	for k, v := range a.authHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil && resp != nil {
		resp.Body.Close()
	}
}

// buildOpencodePrompt appends schema instructions to the prompt. opencode
// 2.x has no native json_schema output mode, so this is the only structured
// output path: the model is told the schema and finalizeTextResult validates
// what it wrote against it.
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
