package main

import (
	"bytes"
	"context"
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
	"syscall"
	"time"
)

// recordOpencode boots the real `opencode serve` server and drives it
// the same way no-mistakes does: open SSE, POST /session, POST a message,
// wait for session.idle. Every byte read from the server is teed to disk
// so the fake can replay the exact wire shape.
//
// Output layout under <out>/<flavour>/:
//
//	session.json     — POST /session response
//	sse.txt          — raw SSE bytes from /global/event up to session.idle
//	message.json     — POST /session/{id}/message response
//
// Two flavours are recorded: "structured" (json_schema format) and
// "plain" (no format).
func recordOpencode(ctx context.Context, out string, args []string) int {
	bin, forward := splitBinArgs(args, "opencode")

	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

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

	if err := srvCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start opencode: %v\n", err)
		return 1
	}
	defer func() {
		// Best-effort SIGTERM, then kill if it lingers.
		_ = srvCmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = srvCmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = srvCmd.Process.Kill()
		}
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitHealth(ctx, baseURL); err != nil {
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
		if err := captureOpencodeFlavour(ctx, baseURL, dir, f.prompt, f.schema); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	fmt.Fprintf(os.Stderr, "opencode fixtures written to %s\n", out)
	return 0
}

func captureOpencodeFlavour(ctx context.Context, baseURL, dir, prompt, schema string) error {
	// Create session.
	tmp, err := os.MkdirTemp("", "recordopencode-*")
	if err != nil {
		return fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(tmp)

	sessionBody := map[string]any{
		"directory": tmp,
		"permission": []map[string]string{
			{"permission": "*", "pattern": "*", "action": "allow"},
		},
	}
	sessionResp, sessionRaw, err := postJSON(ctx, baseURL+"/session", sessionBody)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), sessionRaw, 0o644); err != nil {
		return err
	}
	var sess struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(sessionResp, &sess); err != nil {
		return fmt.Errorf("parse session: %w", err)
	}

	// Open SSE in the background, capturing to a buffer until session.idle.
	sseCtx, sseCancel := context.WithCancel(ctx)
	defer sseCancel()
	sseDone := make(chan error, 1)
	var sseBuf bytes.Buffer
	go func() {
		sseDone <- streamSSE(sseCtx, baseURL+"/global/event", &sseBuf)
	}()

	// Tiny gap so the SSE listener is registered server-side before the
	// message kicks off events.
	time.Sleep(200 * time.Millisecond)

	msgBody := map[string]any{
		"role":  "user",
		"parts": []map[string]string{{"type": "text", "text": prompt}},
	}
	if schema != "" {
		msgBody["format"] = map[string]any{
			"type":       "json_schema",
			"schema":     json.RawMessage(schema),
			"retryCount": 1,
		}
	}
	_, msgRaw, err := postJSON(ctx, baseURL+"/session/"+sess.ID+"/message", msgBody)
	if err != nil {
		sseCancel()
		<-sseDone
		return fmt.Errorf("send message: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "message.json"), msgRaw, 0o644); err != nil {
		return err
	}

	// Drain SSE: keep reading for up to 5s after the message returns,
	// or until session.idle appears in the buffer.
	deadline := time.Now().Add(5 * time.Second)
	idleSeen := false
	for {
		if bytes.Contains(sseBuf.Bytes(), []byte("\"session.idle\"")) {
			idleSeen = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	sseCancel()
	if err := <-sseDone; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("capture SSE: %w", err)
	}
	if !idleSeen {
		return fmt.Errorf("capture SSE: missing session.idle event")
	}

	if err := os.WriteFile(filepath.Join(dir, "sse.txt"), sseBuf.Bytes(), 0o644); err != nil {
		return err
	}

	// Strip personal paths from every captured artefact.
	for _, name := range []string{"session.json", "message.json", "sse.txt"} {
		if err := scrubFile(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("scrub %s: %w", name, err)
		}
	}

	// Best-effort delete session.
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/session/"+sess.ID, nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	return nil
}

func waitHealth(ctx context.Context, baseURL string) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/global/health", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("opencode never became healthy at %s", baseURL)
}

func postJSON(ctx context.Context, url string, body any) (parsed []byte, raw []byte, err error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return raw, raw, fmt.Errorf("%s -> %d: %s", url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, raw, nil
}

func streamSSE(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
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
