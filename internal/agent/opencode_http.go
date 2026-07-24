package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

func (a *opencodeAgent) ensureServer(ctx context.Context, cwd string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return a.server.baseURL(), nil
	}
	// Under the trusted opt-out, the serve argv must carry
	// --no-project-instructions. Older OpenCode binaries that do not recognize
	// the flag exit non-zero and print help to stderr (verified on 1.18.4),
	// so a blind start surfaces only "server exited before becoming healthy:
	// exit status 1" - not a concrete diagnostic. Probe the binary's
	// --help output for the flag BEFORE starting the server so an unsupported
	// binary stops with a clear, actionable error rather than a generic
	// health-check timeout. This is the OpenCode analogue of
	// probeRovoDevSupport, scoped to the opt-out path so normal (non-review)
	// OpenCode behavior is unchanged.
	if a.disableProjectSettings {
		if err := probeOpencodeNoProjectInstructions(ctx, a.bin); err != nil {
			return "", err
		}
	}
	port, err := getAvailablePort()
	if err != nil {
		return "", fmt.Errorf("opencode port: %w", err)
	}
	args := buildOpencodeServeArgs(a.extraArgs, port, a.disableProjectSettings)
	srv, err := startServerWithPort(ctx, "opencode", a.bin, args, cwd, "/global/health", port)
	if err != nil {
		return "", fmt.Errorf("opencode server: %w", err)
	}
	a.server = srv
	return srv.baseURL(), nil
}

// probeOpencodeNoProjectInstructions reports whether the OpenCode binary at bin
// supports the --no-project-instructions serve flag. It scans `opencode serve
// --help` for the flag name: yargs lists recognized boolean flags in the help
// output, so an older binary that rejects the flag omits it. The probe is
// side-effect-free (help prints and exits 0) and bounded to 5s. A non-zero or
// timed-out probe is surfaced as a concrete diagnostic so the operator knows to
// upgrade OpenCode rather than guessing at a generic health-check timeout.
//
// The env-var analogue OPENCODE_DISABLE_PROJECT_INSTRUCTIONS is intentionally
// NOT used: an older binary would silently ignore an unknown env var, which is
// exactly the failure mode the CLI switch exists to prevent. The switch makes
// an unsupported binary reject the invocation.
var probeOpencodeNoProjectInstructions = func(ctx context.Context, bin string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, bin, "serve", "--help")
	configureManagedServerCmd(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("opencode at %q does not support the --no-project-instructions "+
			"flag required by disable_project_settings (probe `serve --help` failed: %w); "+
			"upgrade OpenCode to a version that supports --no-project-instructions or set "+
			"'agent' to codex or claude in ~/.no-mistakes/config.yaml", bin, err)
	}
	if !opencodeHelpListsNoProjectInstructions(string(output)) {
		return fmt.Errorf("opencode at %q does not support the --no-project-instructions "+
			"flag required by disable_project_settings (the flag is absent from "+
			"`serve --help`); upgrade OpenCode to a version that supports "+
			"--no-project-instructions or set 'agent' to codex or claude in "+
			"~/.no-mistakes/config.yaml", bin)
	}
	return nil
}

// opencodeHelpListsNoProjectInstructions reports whether the `opencode serve
// --help` text advertises the --no-project-instructions flag. It matches the
// flag as a whole-word token so a similarly-named flag like
// --no-project-instructions-foo does not false-positive, and tolerates the
// leading "--" yargs emits.
func opencodeHelpListsNoProjectInstructions(helpText string) bool {
	const flag = "--no-project-instructions"
	for _, line := range strings.Split(helpText, "\n") {
		// yargs indents flag rows; the flag name appears after the leading "--".
		// Match "--no-project-instructions" as a whole-word token so a
		// similarly-named flag does not false-match: the token must be
		// bounded by non-flag-name characters (whitespace, ',', '[', or end).
		// A bare substring match would false-positive on
		// --no-project-instructions-foo.
		if helpLineHasFlagToken(line, flag) {
			return true
		}
	}
	return false
}

// helpLineHasFlagToken reports whether line contains flag as a whole-word
// token. A token boundary is any char that cannot appear inside a yargs long
// flag name (anything other than [A-Za-z0-9-]) or the start/end of the line,
// so "--no-project-instructions-foo" does not match "--no-project-instructions".
func helpLineHasFlagToken(line, flag string) bool {
	idx := strings.Index(line, flag)
	if idx < 0 {
		return false
	}
	end := idx + len(flag)
	leftOK := idx == 0 || !isFlagNameChar(line[idx-1])
	rightOK := end == len(line) || !isFlagNameChar(line[end])
	return leftOK && rightOK
}

// isFlagNameChar reports whether c may appear inside a yargs long flag name
// (letters, digits, and hyphens). A hyphen is included because yargs long
// flags are kebab-case; any other char (space, comma, '[', '=', ':') is a
// boundary that delimits the flag token.
func isFlagNameChar(c byte) bool {
	return c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// buildOpencodeServeArgs builds `opencode serve`'s argv with user-supplied
// extras inserted after the "serve" subcommand and before the managed flags.
//
// NOTE: --model is NOT a valid `opencode serve` flag (it is a `run`/TUI flag);
// passing it makes yargs print help and exit. opencodeExtractModel (called in
// NewWithOptions) strips --model from extraArgs before they reach this function
// and routes the model to the session creation API instead. Other operator
// extraArgs (e.g. --log-level) are preserved.
//
// Under the trusted opt-out (disableProjectSettings), the neutralization
// flags --no-project-instructions and --pure are appended LAST so they win
// over any operator override (yargs last-wins):
//   - --no-project-instructions disables project instructions (AGENTS.md /
//     CLAUDE.md), project config (.opencode/), and project skill discovery.
//   - --pure disables external/project plugins.
//
// Both are defense-in-depth: the pre-flight capability probe in ensureServer
// already refused an older binary that lacks --no-project-instructions, and
// even an older binary that somehow passed the probe would reject the unknown
// flag and exit non-zero.
func buildOpencodeServeArgs(extraArgs []string, port int, disableProjectSettings bool) []string {
	args := make([]string, 0, len(extraArgs)+8)
	args = append(args, "serve")
	args = append(args, extraArgs...)
	args = append(args, "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--print-logs")
	if disableProjectSettings {
		// Managed neutralization flags are appended last so yargs last-wins
		// enforces them over any operator attempt to defeat the opt-out. They
		// are NOT taken from extraArgs (operator cannot remove them); if the
		// operator already passed --pure we still re-assert it harmlessly.
		args = append(args, "--no-project-instructions", "--pure")
	}
	return args
}

// opencodeExtractModel removes --model <value> (and --model=<value>) from
// extraArgs and returns the remaining args plus the model value. `opencode
// serve` does not accept --model (it is a `run`/TUI flag); passing it makes
// yargs print help and exit 0, breaking the server. The model is instead
// passed to the session creation API (see createSession). When --model is
// absent, the remaining args are returned unchanged with an empty model.
//
// A bare trailing `--model` with no following value is a malformed operator
// config (agent_args_override); rather than passing the dangling flag through
// to opencode serve (where yargs would either reject it or silently consume
// the next flag as its value), it is surfaced as an explicit error here so
// the operator gets a clear "missing --model value" diagnostic at agent
// construction time.
func opencodeExtractModel(extraArgs []string) ([]string, string, error) {
	var model string
	out := make([]string, 0, len(extraArgs))
	for i := 0; i < len(extraArgs); i++ {
		arg := extraArgs[i]
		if arg == "--model" {
			if i+1 >= len(extraArgs) {
				return nil, "", fmt.Errorf("opencode agent_args_override: --model requires a value " +
					"(e.g. --model ollama-cloud/glm-5.2); fix agent_args_override.opencode in " +
					"~/.no-mistakes/config.yaml")
			}
			model = extraArgs[i+1]
			i++ // skip the value
			continue
		}
		if strings.HasPrefix(arg, "--model=") {
			model = strings.TrimPrefix(arg, "--model=")
			continue
		}
		out = append(out, arg)
	}
	return out, model, nil
}

func (a *opencodeAgent) createSession(ctx context.Context, baseURL, cwd string) (string, error) {
	body := map[string]any{
		"directory": cwd,
		"permission": []map[string]string{
			{"permission": "*", "pattern": "*", "action": "allow"},
		},
	}
	// Pass the operator-pinned model to the session creation API when set.
	// opencode serve does not accept --model as a CLI flag, so the model is
	// selected per-session instead. The model format is "provider/model" (e.g.
	// "ollama-cloud/glm-5.2"), which maps to model.id=model, model.providerID=
	// provider. An invalid format (no slash) fails loudly here rather than
	// silently falling back to the default model, so a misconfigured pin is
	// obvious instead of a silent footgun.
	if a.sessionModel != "" {
		providerID, modelID, ok := parseOpencodeModel(a.sessionModel)
		if !ok {
			return "", fmt.Errorf("opencode create session: operator --model %q is not in 'provider/model' format "+
				"(e.g. 'ollama-cloud/glm-5.2'); fix agent_args_override.opencode in ~/.no-mistakes/config.yaml",
				a.sessionModel)
		}
		body["model"] = map[string]any{
			"id":         modelID,
			"providerID": providerID,
		}
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

// parseOpencodeModel splits a "provider/model" string into its provider and
// model parts. Returns ok=false if the format is invalid (no slash).
func parseOpencodeModel(model string) (providerID, modelID string, ok bool) {
	idx := strings.Index(model, "/")
	if idx <= 0 || idx >= len(model)-1 {
		return "", "", false
	}
	return model[:idx], model[idx+1:], true
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
