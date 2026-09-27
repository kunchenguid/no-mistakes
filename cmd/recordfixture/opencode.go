package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// recordOpencode boots the real `opencode serve` (opencode 2.x) and drives it
// the same way no-mistakes does: authenticate with a generated server
// password, POST /api/session, open the /api/event stream, admit a prompt,
// wait for the turn's execution end, read the message list back. Every byte
// read from the server is teed to disk so the fake can replay the exact wire
// shape.
//
// Output layout under <out>/<flavour>/:
//
//	session.json     — POST /api/session response
//	sse.txt          — raw SSE bytes from /api/event for the session, up to
//	                   its session.execution.{succeeded,failed,interrupted}
//	messages.json    — GET /api/session/{id}/message response after the turn
//
// Two flavours are recorded: "structured" (a JSON schema in the prompt, the
// only structured-output mode opencode 2.x has) and "plain".
func recordOpencode(ctx context.Context, out string, args []string) int {
	bin, forward := splitBinArgs(args, "opencode")

	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	password, err := randomPassword()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	auth := opencodeAuth(password)

	srvArgs := []string{
		"serve",
		"--hostname", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--print-logs",
	}
	srvArgs = append(srvArgs, forward...)
	srvCmd := exec.CommandContext(ctx, bin, srvArgs...)
	srvCmd.SysProcAttr = newProcAttr() // own process group so we can SIGTERM cleanly
	srvCmd.Stdout = os.Stderr
	srvCmd.Stderr = os.Stderr
	// Both variables, for the reason the adapter sets both: opencode reads
	// OPENCODE_PASSWORD first, and an operator may have it set for another
	// server.
	srvCmd.Env = append(os.Environ(), "OPENCODE_SERVER_PASSWORD="+password, "OPENCODE_PASSWORD="+password)

	if err := srvCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start opencode: %v\n", err)
		return 1
	}
	defer func() {
		_ = terminateCmd(srvCmd, 3*time.Second)
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitHealth(ctx, baseURL, auth); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	flavours := []struct {
		name   string
		schema string
		prompt string
	}{
		{
			name: "structured",
			schema: `{"type":"object","properties":{` +
				`"findings":{"type":"array","items":{"type":"object"}},` +
				`"risk_level":{"type":"string","enum":["low","medium","high"]},` +
				`"risk_rationale":{"type":"string"}},` +
				`"required":["findings","risk_level","risk_rationale"]}`,
			prompt: "Reply with structured JSON: empty findings array, risk_level=low, one short risk_rationale.",
		},
		{
			name:   "plain",
			schema: "",
			prompt: "Reply with the literal word OK and nothing else.",
		},
	}
	for _, f := range flavours {
		dir := filepath.Join(out, f.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "recording opencode/%s → %s\n", f.name, dir)
		if err := captureOpencodeFlavour(ctx, baseURL, auth, dir, f.prompt, f.schema); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	fmt.Fprintf(os.Stderr, "opencode fixtures written to %s\n", out)
	return 0
}

func randomPassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("server password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// opencodeAuth is the basic-auth header opencode 2.x expects: the fixed
// username "opencode" and the password the serve process was handed.
func opencodeAuth(password string) map[string]string {
	return map[string]string{
		"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+password)),
	}
}

// opencodeSchemaPrompt appends the same schema instructions the adapter's
// buildOpencodePrompt does, so the recording exercises the prompt the
// pipeline really sends. The fake patches the assistant text anyway; this
// only keeps the recorded turn representative.
func opencodeSchemaPrompt(prompt, schema string) string {
	if schema == "" {
		return prompt
	}
	return strings.Join([]string{
		prompt,
		"",
		"When you finish, reply with only valid JSON.",
		"Do not wrap the JSON in markdown fences.",
		"Do not include any prose before or after the JSON.",
		"The JSON must match this schema exactly: " + schema,
	}, "\n")
}

// opencodeTurnWait bounds how long a recording waits for the real model to
// finish a turn once the prompt was admitted.
const opencodeTurnWait = 3 * time.Minute

func captureOpencodeFlavour(ctx context.Context, baseURL string, auth map[string]string, dir, prompt, schema string) error {
	tmp, err := os.MkdirTemp("", "recordopencode-*")
	if err != nil {
		return fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(tmp)

	sessionBody := map[string]any{
		"location": map[string]string{"directory": tmp},
		"permissions": []map[string]string{
			{"action": "*", "resource": "*", "effect": "allow"},
		},
	}
	sessionRaw, err := postJSON(ctx, baseURL+"/api/session", auth, sessionBody)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), sessionRaw, 0o644); err != nil {
		return err
	}
	var sess struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(sessionRaw, &sess); err != nil {
		return fmt.Errorf("parse session: %w", err)
	}
	if sess.Data.ID == "" {
		return fmt.Errorf("parse session: response carried no data.id: %s", sessionRaw)
	}
	sessionID := sess.Data.ID

	// Open SSE in the background, capturing to a synchronized buffer until
	// the turn's execution end.
	sseCtx, sseCancel := context.WithCancel(ctx)
	defer sseCancel()
	sseDone := make(chan error, 1)
	sseReady := make(chan struct{})
	sseCapture := newOpencodeSSECapture(sessionID)
	go func() {
		sseDone <- streamSSE(sseCtx, baseURL+"/api/event", auth, sseCapture, sseReady)
	}()

	readyCtx, readyCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := waitForSSEReady(readyCtx, sseReady); err != nil {
		readyCancel()
		sseCancel()
		if streamErr := <-sseDone; streamErr != nil && !errors.Is(streamErr, context.Canceled) {
			return fmt.Errorf("capture SSE: %w", streamErr)
		}
		return fmt.Errorf("capture SSE: %w", err)
	}
	readyCancel()

	promptBody := map[string]any{"text": opencodeSchemaPrompt(prompt, schema)}
	if _, err := postJSON(ctx, baseURL+"/api/session/"+sessionID+"/prompt", auth, promptBody); err != nil {
		sseCancel()
		<-sseDone
		return fmt.Errorf("send prompt: %w", err)
	}

	endCtx, endCancel := context.WithTimeout(ctx, opencodeTurnWait)
	waitErr := sseCapture.WaitForEnd(endCtx)
	endCancel()
	sseCancel()
	streamErr := <-sseDone
	if waitErr != nil {
		// The turn never reported an end within the wait: whether the
		// stream was then cancelled by us or by the caller's deadline, the
		// actionable fact is the missing end event.
		return fmt.Errorf("capture SSE: missing session.execution end event (%v)", waitErr)
	}
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		return fmt.Errorf("capture SSE: %w", streamErr)
	}

	if err := os.WriteFile(filepath.Join(dir, "sse.txt"), sseCapture.Bytes(), 0o644); err != nil {
		return err
	}

	messagesRaw, err := getJSON(ctx, baseURL+"/api/session/"+sessionID+"/message", auth)
	if err != nil {
		return fmt.Errorf("read messages: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "messages.json"), messagesRaw, 0o644); err != nil {
		return err
	}

	// Strip personal paths from every captured artefact.
	for _, name := range []string{"session.json", "sse.txt", "messages.json"} {
		if err := scrubFile(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("scrub %s: %w", name, err)
		}
	}

	// Best-effort delete session.
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/api/session/"+sessionID, nil)
	if req != nil {
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return nil
}

// opencodeSSECapture keeps the frames of one session from the global event
// stream and reports when that session's turn ended.
type opencodeSSECapture struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	pending   []byte
	sessionID string
	endSeen   bool
	endCh     chan struct{}
}

func newOpencodeSSECapture(sessionID string) *opencodeSSECapture {
	return &opencodeSSECapture{sessionID: sessionID, endCh: make(chan struct{})}
}

func (c *opencodeSSECapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	c.pending = append(c.pending, p...)
	for !c.endSeen {
		idx := bytes.Index(c.pending, []byte("\n\n"))
		if idx < 0 {
			break
		}
		event := c.pending[:idx]
		c.pending = c.pending[idx+2:]
		eventType, eventSession := sseEventIdentity(event)
		if eventSession != "" && eventSession != c.sessionID {
			continue
		}
		if _, err := c.buf.Write(event); err != nil {
			return n, err
		}
		if _, err := c.buf.Write([]byte("\n\n")); err != nil {
			return n, err
		}
		if eventSession == c.sessionID && isOpencodeExecutionEnd(eventType) {
			c.endSeen = true
			close(c.endCh)
		}
	}
	return n, nil
}

func isOpencodeExecutionEnd(eventType string) bool {
	switch eventType {
	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted":
		return true
	}
	return false
}

// sseEventIdentity reads the type and, for session events, the session id of
// one SSE event's data frame (opencode 2.x: {"type", "data": {"sessionID"}}).
// Events that carry no session id (server.connected, project.updated, ...)
// report an empty session and are kept, because the real stream carries them
// too and the fake replays the stream verbatim.
func sseEventIdentity(event []byte) (eventType, sessionID string) {
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var payload struct {
			Type string `json:"type"`
			Data struct {
				SessionID string `json:"sessionID"`
			} `json:"data"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &payload); err != nil {
			continue
		}
		return payload.Type, payload.Data.SessionID
	}
	return "", ""
}

func (c *opencodeSSECapture) WaitForEnd(ctx context.Context) error {
	select {
	case <-c.endDone():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *opencodeSSECapture) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func (c *opencodeSSECapture) endDone() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endCh
}

// waitHealth polls the authenticated /api/info route: it is the one request
// the SPA fallback cannot satisfy, so a 200 means the v2 API is up and the
// password was adopted.
func waitHealth(ctx context.Context, baseURL string, auth map[string]string) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/info", nil)
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				return nil
			case http.StatusNotFound:
				return fmt.Errorf("opencode at %s has no /api/info: recording needs opencode 2.x", baseURL)
			case http.StatusUnauthorized:
				return fmt.Errorf("opencode at %s rejected the generated server password", baseURL)
			}
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("opencode never became healthy at %s", baseURL)
}

func postJSON(ctx context.Context, url string, headers map[string]string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return doRaw(req, headers)
}

func getJSON(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return doRaw(req, headers)
}

func doRaw(req *http.Request, headers map[string]string) ([]byte, error) {
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return raw, fmt.Errorf("%s %s -> %d: %s", req.Method, req.URL, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func waitForSSEReady(ctx context.Context, ready <-chan struct{}) error {
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func streamSSE(ctx context.Context, url string, headers map[string]string, w io.Writer, ready chan<- struct{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("%s -> %d: read error body: %w", url, resp.StatusCode, readErr)
		}
		return fmt.Errorf("%s -> %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	close(ready)
	_, err = io.Copy(w, resp.Body)
	return err
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
