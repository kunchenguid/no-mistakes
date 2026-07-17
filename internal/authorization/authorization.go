// Package authorization implements the opt-in external authorization protocol
// used by orchestrators that manage no-mistakes runs. Unmanaged callers never
// contact a verifier and retain the ordinary standalone behavior.
package authorization

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	ProtocolVersion = "1"

	OperationRun         Operation = "run"
	OperationGatePush    Operation = "gate-push"
	OperationAgentLaunch Operation = "agent-launch"

	defaultTimeout  = 2 * time.Second
	maxResponseBody = 32 << 10
)

// Operation is a protected no-mistakes boundary.
type Operation string

// Context is the in-memory authorization capability for one managed runtime.
// Token is intentionally never persisted, logged, placed in git configuration,
// or forwarded to review-agent processes.
type Context struct {
	Managed           bool
	VerifierURL       string
	Token             string
	TaskID            string
	RuntimeGeneration int64
	SessionID         string
	ProjectPath       string
	Repository        string
	WorktreePath      string
	Branch            string
	DurableMode       string
	Timeout           time.Duration
	provider          string
}

// WireContext is the minimum transient context carried over the local daemon
// IPC channel. It must never be written to diagnostics or durable storage.
type WireContext struct {
	Managed           bool   `json:"managed"`
	VerifierURL       string `json:"verifier_url,omitempty"`
	Token             string `json:"token,omitempty"`
	TaskID            string `json:"task_id,omitempty"`
	RuntimeGeneration int64  `json:"runtime_generation,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	ProjectPath       string `json:"project_path,omitempty"`
	Repository        string `json:"repository,omitempty"`
	WorktreePath      string `json:"worktree_path,omitempty"`
	Branch            string `json:"branch,omitempty"`
	DurableMode       string `json:"durable_mode,omitempty"`
	Provider          string `json:"provider,omitempty"`
}

func (w WireContext) String() string {
	return FromWire(&w).String()
}

func (w WireContext) GoString() string {
	return FromWire(&w).String()
}

// Request is protocol version 1's exact authorization request.
type Request struct {
	ProtocolVersion   string    `json:"protocolVersion"`
	RequestID         string    `json:"requestId"`
	Operation         Operation `json:"operation"`
	TaskID            string    `json:"taskId"`
	RuntimeGeneration int64     `json:"runtimeGeneration"`
	SessionID         string    `json:"sessionId"`
	ProjectPath       string    `json:"projectPath"`
	Repository        string    `json:"repository"`
	WorktreePath      string    `json:"worktreePath"`
	Branch            string    `json:"branch"`
	DurableMode       string    `json:"durableMode"`
}

// Response must echo the complete request scope. RequestID is a one-use nonce;
// the verifier owns replay detection and returns a denial for a reused value.
type Response struct {
	ProtocolVersion   string    `json:"protocolVersion"`
	RequestID         string    `json:"requestId"`
	Operation         Operation `json:"operation"`
	TaskID            string    `json:"taskId"`
	RuntimeGeneration int64     `json:"runtimeGeneration"`
	SessionID         string    `json:"sessionId"`
	ProjectPath       string    `json:"projectPath"`
	Repository        string    `json:"repository"`
	WorktreePath      string    `json:"worktreePath"`
	Branch            string    `json:"branch"`
	DurableMode       string    `json:"durableMode"`
	Allowed           bool      `json:"allowed"`
	Reason            string    `json:"reason"`
}

// FromEnvironment recognizes Perch's authenticated hook environment and a
// provider-neutral NO_MISTAKES_AUTHORIZATION_* equivalent. Any partial managed
// context fails closed instead of silently reverting to standalone behavior.
func FromEnvironment(environ []string) (Context, error) {
	env := envMap(environ)
	perch := hasAnyPrefix(env, "PERCH_TASK_", "PERCH_HOOK_", "PERCH_SESSION_ID", "PERCH_RUNTIME_GENERATION")
	generic := env["NO_MISTAKES_AUTHORIZATION_MODE"] != "" || hasAnyPrefix(env, "NO_MISTAKES_AUTHORIZATION_")
	if !perch && !generic {
		return Context{}, nil
	}

	var c Context
	if perch {
		c = Context{
			Managed:      true,
			VerifierURL:  perchVerifierURL(env["PERCH_HOOK_URL"]),
			Token:        env["PERCH_HOOK_TOKEN"],
			TaskID:       env["PERCH_TASK_ID"],
			SessionID:    env["PERCH_SESSION_ID"],
			ProjectPath:  env["PERCH_TASK_PROJECT"],
			Repository:   env["PERCH_TASK_REPOSITORY"],
			WorktreePath: env["PERCH_TASK_WORKTREE"],
			Branch:       env["PERCH_TASK_BRANCH"],
			DurableMode:  env["PERCH_TASK_MODE"],
			provider:     "perch",
		}
		generation, err := strconv.ParseInt(env["PERCH_RUNTIME_GENERATION"], 10, 64)
		if err != nil {
			return Context{}, errors.New("managed authorization denied: invalid runtime generation")
		}
		c.RuntimeGeneration = generation
	} else {
		if env["NO_MISTAKES_AUTHORIZATION_MODE"] != "managed" {
			return Context{}, errors.New("managed authorization denied: invalid mode")
		}
		c = Context{
			Managed:      true,
			VerifierURL:  env["NO_MISTAKES_AUTHORIZATION_URL"],
			Token:        env["NO_MISTAKES_AUTHORIZATION_TOKEN"],
			TaskID:       env["NO_MISTAKES_AUTHORIZATION_TASK_ID"],
			SessionID:    env["NO_MISTAKES_AUTHORIZATION_SESSION_ID"],
			ProjectPath:  env["NO_MISTAKES_AUTHORIZATION_PROJECT"],
			Repository:   env["NO_MISTAKES_AUTHORIZATION_REPOSITORY"],
			WorktreePath: env["NO_MISTAKES_AUTHORIZATION_WORKTREE"],
			Branch:       env["NO_MISTAKES_AUTHORIZATION_BRANCH"],
			DurableMode:  env["NO_MISTAKES_AUTHORIZATION_DURABLE_MODE"],
			provider:     "generic",
		}
		generation, err := strconv.ParseInt(env["NO_MISTAKES_AUTHORIZATION_RUNTIME_GENERATION"], 10, 64)
		if err != nil {
			return Context{}, errors.New("managed authorization denied: invalid runtime generation")
		}
		c.RuntimeGeneration = generation
	}
	if err := c.validate(); err != nil {
		return Context{}, err
	}
	c.ProjectPath = canonicalPath(c.ProjectPath)
	c.Repository = CanonicalRepository(c.Repository)
	c.WorktreePath = canonicalPath(c.WorktreePath)
	return c, nil
}

// IsManagedEnvironment reports whether any explicit managed-provider marker is
// present. It is used by mutation surfaces such as self-update, which must
// refuse even when the rest of a damaged managed context is incomplete.
func IsManagedEnvironment(environ []string) bool {
	env := envMap(environ)
	return hasAnyPrefix(env, "PERCH_TASK_", "PERCH_HOOK_", "PERCH_SESSION_ID", "PERCH_RUNTIME_GENERATION") ||
		env["NO_MISTAKES_AUTHORIZATION_MODE"] != "" || hasAnyPrefix(env, "NO_MISTAKES_AUTHORIZATION_")
}

func perchVerifierURL(hookURL string) string {
	base := strings.TrimRight(strings.TrimSpace(hookURL), "/")
	if strings.HasSuffix(base, "/hooks") {
		return base + "/no-mistakes/authorize"
	}
	return base + "/hooks/no-mistakes/authorize"
}

func envMap(environ []string) map[string]string {
	result := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

func hasAnyPrefix(env map[string]string, prefixes ...string) bool {
	for key := range env {
		for _, prefix := range prefixes {
			if key == prefix || strings.HasPrefix(key, prefix) {
				return true
			}
		}
	}
	return false
}

func (c Context) validate() error {
	if !c.Managed {
		return nil
	}
	if c.VerifierURL == "" || c.Token == "" || c.TaskID == "" || c.SessionID == "" || c.ProjectPath == "" || c.Repository == "" || c.WorktreePath == "" || c.Branch == "" || c.DurableMode == "" {
		return errors.New("managed authorization denied: incomplete verifier context")
	}
	parsed, err := url.Parse(c.VerifierURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return errors.New("managed authorization denied: invalid verifier URL")
	}
	if c.RuntimeGeneration < 0 {
		return errors.New("managed authorization denied: invalid runtime generation")
	}
	return nil
}

// Authorize performs a fresh verifier call for exactly one protected operation.
// Decisions are deliberately never cached.
func (c Context) Authorize(ctx context.Context, operation Operation) error {
	if !c.Managed {
		return nil
	}
	if err := c.validate(); err != nil {
		return err
	}
	if operation != OperationRun && operation != OperationGatePush && operation != OperationAgentLaunch {
		return errors.New("managed authorization denied: unsupported operation")
	}
	requestID, err := newRequestID()
	if err != nil {
		return errors.New("managed authorization denied: request identity unavailable")
	}
	request := Request{
		ProtocolVersion: ProtocolVersion, RequestID: requestID, Operation: operation,
		TaskID: c.TaskID, RuntimeGeneration: c.RuntimeGeneration, SessionID: c.SessionID,
		ProjectPath: canonicalPath(c.ProjectPath), Repository: CanonicalRepository(c.Repository), WorktreePath: canonicalPath(c.WorktreePath),
		Branch: c.Branch, DurableMode: c.DurableMode,
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return errors.New("managed authorization denied: request encoding failed")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.VerifierURL, bytes.NewReader(payload))
	if err != nil {
		return errors.New("managed authorization denied: verifier request failed")
	}
	httpRequest.Header.Set("content-type", "application/json")
	if c.provider == "perch" {
		httpRequest.Header.Set("x-perch-session", c.SessionID)
		httpRequest.Header.Set("x-perch-token", c.Token)
	} else {
		httpRequest.Header.Set("authorization", "Bearer "+c.Token)
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return errors.New("managed authorization denied: verifier unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
	if err != nil {
		return errors.New("managed authorization denied: verifier response unreadable")
	}
	var decision Response
	if err := json.Unmarshal(body, &decision); err != nil {
		return errors.New("managed authorization denied: malformed verifier response")
	}
	if err := validateEcho(request, decision); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || !decision.Allowed || decision.DurableMode != "no-mistakes" {
		return fmt.Errorf("managed authorization denied: %s", safeReason(decision.Reason, c.Token))
	}
	return nil
}

func validateEcho(request Request, response Response) error {
	if response.ProtocolVersion != request.ProtocolVersion || response.RequestID != request.RequestID ||
		response.Operation != request.Operation || response.TaskID != request.TaskID ||
		response.RuntimeGeneration != request.RuntimeGeneration || response.SessionID != request.SessionID ||
		response.ProjectPath != request.ProjectPath || response.Repository != request.Repository ||
		response.WorktreePath != request.WorktreePath || response.Branch != request.Branch {
		return errors.New("managed authorization denied: verifier scope mismatch")
	}
	return nil
}

func safeReason(reason, credential string) string {
	if reason == "" || len(reason) > 64 || (credential != "" && strings.Contains(reason, credential)) {
		return "not_authorized"
	}
	for _, char := range reason {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' && char != '-' && char != '.' {
			return "not_authorized"
		}
	}
	return reason
}

func newRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

// ValidateLocalScope rejects obvious task/repository/worktree/branch replay
// before contacting the verifier. The verifier remains authoritative.
func (c Context) ValidateLocalScope(projectPath, repository, worktreePath, branch string) error {
	if !c.Managed {
		return nil
	}
	if canonicalPath(c.ProjectPath) != canonicalPath(projectPath) || canonicalPath(c.WorktreePath) != canonicalPath(worktreePath) ||
		CanonicalRepository(c.Repository) != CanonicalRepository(repository) || c.Branch != branch || c.DurableMode != "no-mistakes" {
		return errors.New("managed authorization denied: local scope mismatch")
	}
	return nil
}

func canonicalPath(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	path, err := filepath.Abs(value)
	if err != nil {
		return filepath.Clean(value)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// CanonicalRepository normalizes common Git URL forms without credentials.
func CanonicalRepository(value string) string {
	value = strings.TrimSpace(strings.TrimSuffix(value, ".git"))
	if strings.Contains(value, "://") {
		if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
			return strings.ToLower(parsed.Host) + "/" + strings.TrimPrefix(parsed.Path, "/")
		}
	}
	if at := strings.LastIndex(value, "@"); at >= 0 {
		value = value[at+1:]
	}
	value = strings.Replace(value, ":", "/", 1)
	return strings.ToLower(strings.TrimPrefix(value, "/"))
}

func (c Context) Wire() *WireContext {
	if !c.Managed {
		return nil
	}
	return &WireContext{Managed: true, VerifierURL: c.VerifierURL, Token: c.Token, TaskID: c.TaskID,
		RuntimeGeneration: c.RuntimeGeneration, SessionID: c.SessionID, ProjectPath: canonicalPath(c.ProjectPath),
		Repository: CanonicalRepository(c.Repository), WorktreePath: canonicalPath(c.WorktreePath), Branch: c.Branch,
		DurableMode: c.DurableMode, Provider: c.provider}
}

func FromWire(w *WireContext) Context {
	if w == nil || !w.Managed {
		return Context{}
	}
	return Context{Managed: true, VerifierURL: w.VerifierURL, Token: w.Token, TaskID: w.TaskID,
		RuntimeGeneration: w.RuntimeGeneration, SessionID: w.SessionID, ProjectPath: w.ProjectPath,
		Repository: w.Repository, WorktreePath: w.WorktreePath, Branch: w.Branch,
		DurableMode: w.DurableMode, provider: w.Provider}
}

type redactedContext struct{ Context }

func (c Context) Redacted() redactedContext {
	c.Token = "<redacted>"
	return redactedContext{Context: c}
}

func (c Context) String() string   { return c.Redacted().String() }
func (c Context) GoString() string { return c.Redacted().String() }
func (c redactedContext) String() string {
	return fmt.Sprintf("authorization.Context{Managed:%t TaskID:%q RuntimeGeneration:%d SessionID:%q ProjectPath:%q Repository:%q WorktreePath:%q Branch:%q DurableMode:%q Token:<redacted>}",
		c.Managed, c.TaskID, c.RuntimeGeneration, c.SessionID, c.ProjectPath, c.Repository, c.WorktreePath, c.Branch, c.DurableMode)
}

// ScrubEnvironment removes managed-orchestrator credentials and scope from a
// child process environment.
func ScrubEnvironment(environ []string) []string {
	result := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PERCH_") || strings.HasPrefix(key, "NO_MISTAKES_AUTHORIZATION_") {
			continue
		}
		result = append(result, entry)
	}
	return result
}
