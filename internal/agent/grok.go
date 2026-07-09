package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// grokAgent spawns the Grok CLI for each invocation. Headless mode uses
// `grok -p <prompt>` with either streaming-json events or --json-schema
// (which implies --output-format json). Schema-mode results prefer a non-empty
// structuredOutput field over text. Lifecycle is codex/pi-shaped: one process
// per Run, no managed server.
type grokAgent struct {
	bin       string
	extraArgs []string
}

func (a *grokAgent) Name() string { return "grok" }

func (a *grokAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "grok", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *grokAgent) Close() error { return nil }

func (a *grokAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	args := a.buildArgs(opts.Prompt, opts.JSONSchema)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("grok start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "grok", pid)

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	var usage TokenUsage
	var text string
	var grokErr string
	var parseErr error
	if len(opts.JSONSchema) > 0 {
		parseErr = parseGrokJSONStdout(ctx, started.stdout, &text, &grokErr)
	} else {
		parseErr = parseGrokStreamingEvents(ctx, started.stdout, opts.OnChunk, &usage, &text, &grokErr)
	}
	if parseErr != nil {
		parseErr = started.waitAfterParseError(parseErr)
		stderrWG.Wait()
		retErr := fmt.Errorf("grok parse events: %w", parseErr)
		emitAgentExited(opts, "grok", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()

	detail := grokErrorDetail(grokErr, string(stderrBuf))
	if waitErr != nil {
		if detail != "" {
			retErr := fmt.Errorf("grok exited: %w: %s", waitErr, detail)
			emitAgentExited(opts, "grok", pid, retErr)
			return nil, retErr
		}
		retErr := fmt.Errorf("grok exited: %w", waitErr)
		emitAgentExited(opts, "grok", pid, retErr)
		return nil, retErr
	}
	if grokErr != "" {
		retErr := fmt.Errorf("grok reported error: %s", grokErr)
		emitAgentExited(opts, "grok", pid, retErr)
		return nil, retErr
	}

	res, err := finalizeTextResult("grok", text, opts.JSONSchema, usage)
	emitAgentExited(opts, "grok", pid, err)
	return res, err
}

func grokErrorDetail(grokErr, stderr string) string {
	detail := strings.TrimSpace(grokErr)
	stderr = strings.TrimSpace(stderr)
	if detail != "" && stderr != "" {
		return detail + "; " + stderr
	}
	if detail != "" {
		return detail
	}
	return stderr
}

// buildArgs constructs the grok CLI arguments. User-supplied extraArgs (from
// agent_args_override) come first so flags like -m / --effort take effect.
// When a JSON schema is provided, --json-schema is used (which implies
// --output-format json); otherwise streaming-json is requested for OnChunk.
// --always-approve is added unless the user already set an approval mode.
func (a *grokAgent) buildArgs(prompt string, schema json.RawMessage) []string {
	args := make([]string, 0, len(a.extraArgs)+8)
	args = append(args, a.extraArgs...)
	args = append(args, "-p", prompt)
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	} else {
		args = append(args, "--output-format", "streaming-json")
	}
	if !grokUserSetApproval(a.extraArgs) {
		args = append(args, "--always-approve")
	}
	return args
}

// grokUserSetApproval reports whether extraArgs already control tool
// auto-approval, in which case buildArgs skips its default --always-approve.
func grokUserSetApproval(extraArgs []string) bool {
	for _, arg := range extraArgs {
		switch {
		case arg == "--always-approve",
			arg == "--yolo":
			return true
		case arg == "--permission-mode",
			strings.HasPrefix(arg, "--permission-mode="):
			return true
		}
	}
	return false
}

// grokStreamEvent is one newline-delimited JSON event from
// --output-format streaming-json.
type grokStreamEvent struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Message string `json:"message,omitempty"`
}

// parseGrokStreamingEvents reads streaming-json lines, streams text chunks to
// onChunk, accumulates the final assistant text, and records error messages.
// Thought events are ignored for the final text (they are internal reasoning).
func parseGrokStreamingEvents(
	ctx context.Context,
	r io.Reader,
	onChunk func(string),
	usage *TokenUsage,
	text *string,
	grokErr *string,
) error {
	_ = usage // Grok headless streaming events do not currently carry token usage.
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), claudeScannerMaxTokenSize)

	var b strings.Builder
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event grokStreamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}

		switch event.Type {
		case "text":
			if event.Data == "" {
				continue
			}
			b.WriteString(event.Data)
			if onChunk != nil {
				onChunk(event.Data)
			}
		case "error":
			if msg := firstNonEmpty(event.Message, event.Data); msg != "" && grokErr != nil {
				*grokErr = msg
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if text != nil {
		*text = b.String()
	}
	return nil
}

// parseGrokJSONStdout reads the single JSON object emitted by
// --output-format json (also implied by --json-schema).
func parseGrokJSONStdout(ctx context.Context, r io.Reader, text, grokErr *string) error {
	done := make(chan struct{})
	var raw []byte
	var readErr error
	go func() {
		defer close(done)
		raw, readErr = io.ReadAll(r)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	if readErr != nil {
		return readErr
	}

	parsedText, parsedErr, err := parseGrokJSONResult(raw)
	if err != nil {
		return err
	}
	if text != nil {
		*text = parsedText
	}
	if grokErr != nil {
		*grokErr = parsedErr
	}
	return nil
}

// parseGrokJSONResult decodes a headless --output-format json payload.
// Success: {"text":"...","stopReason":"..."} and, with --json-schema,
// optionally {"structuredOutput":{...}}. Failure: {"type":"error","message":"..."}.
// Prefer non-empty structuredOutput (native constrained JSON) over text so
// schema-mode does not fail when text is empty or prose.
func parseGrokJSONResult(raw []byte) (text, grokErr string, err error) {
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return "", "", nil
	}

	var envelope struct {
		Type             string          `json:"type"`
		Message          string          `json:"message"`
		Text             string          `json:"text"`
		StopReason       string          `json:"stopReason"`
		StructuredOutput json.RawMessage `json:"structuredOutput"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return "", "", fmt.Errorf("decode json result: %w", err)
	}
	structured := bytesTrimSpace(envelope.StructuredOutput)
	hasStructured := len(structured) > 0 && !bytes.Equal(structured, []byte("null"))
	if envelope.Type == "error" || (envelope.Message != "" && envelope.Text == "" && !hasStructured && envelope.StopReason == "") {
		return "", firstNonEmpty(envelope.Message, "unknown error"), nil
	}
	if hasStructured {
		return string(structured), "", nil
	}
	return envelope.Text, "", nil
}

func bytesTrimSpace(b []byte) []byte {
	return bytes.TrimSpace(b)
}
