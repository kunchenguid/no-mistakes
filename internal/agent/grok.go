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

// grokScannerMaxTokenSize matches the buffer used by other JSONL native
// agents (codex/pi) for large single-line events.
const grokScannerMaxTokenSize = 256 * 1024 * 1024

// grokAgent spawns the Grok CLI for each invocation. Headless mode uses
// `grok -p <prompt>` with either streaming-json events or --json-schema
// (which implies --output-format json). Lifecycle is codex/pi-shaped: one
// process per Run, no managed server.
//
// Schema mode follows the same shape as Claude's native structured field:
// prefer non-empty structuredOutput as Result.Output, otherwise fall back
// to text + finalizeTextResult like the other text-parsed agents.
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
	var structured json.RawMessage
	var grokErr string
	var parseErr error
	if len(opts.JSONSchema) > 0 {
		parseErr = parseGrokJSONStdout(ctx, started.stdout, &text, &structured, &grokErr)
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

	res, err := finalizeGrokResult(text, structured, opts.JSONSchema, usage)
	emitAgentExited(opts, "grok", pid, err)
	return res, err
}

// finalizeGrokResult prefers non-empty structuredOutput as Result.Output
// (Claude-shaped) when it validates under the same textValidationSchema
// rules as finalizeTextResult (optional nulls allowed). On validation
// failure with recoverable envelope text it falls back via finalizeTextResult;
// with empty text it returns the structured validation error instead of a
// generic empty-text failure. Empty structuredOutput falls back to text.
func finalizeGrokResult(text string, structured json.RawMessage, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	structured = bytes.TrimSpace(structured)
	hasStructured := len(structured) > 0 && !bytes.Equal(structured, []byte("null"))
	if len(schema) > 0 && hasStructured {
		validationSchema, err := textValidationSchema(schema)
		if err != nil {
			return nil, fmt.Errorf("grok structured output: %w", err)
		}
		if err := validateStructuredOutput(structured, validationSchema); err != nil {
			if strings.TrimSpace(text) == "" {
				return nil, fmt.Errorf("grok structured output: %w", err)
			}
		} else {
			outText := text
			if outText == "" {
				outText = string(structured)
			}
			return &Result{Output: structured, Text: outText, Usage: usage}, nil
		}
	}
	return finalizeTextResult("grok", text, schema, usage)
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
	Type       string `json:"type"`
	Data       string `json:"data,omitempty"`
	Message    string `json:"message,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
}

// grokStopReasonSuccess reports whether a Grok stopReason is a normal
// completed turn. Empty is allowed for payloads that omit the field (error
// envelopes, older fixtures); any other non-EndTurn value is incomplete.
func grokStopReasonSuccess(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "", "EndTurn":
		return true
	default:
		return false
	}
}

// setGrokErrIfEmpty records msg when no more specific error is already set.
func setGrokErrIfEmpty(grokErr *string, msg string) {
	if grokErr == nil || msg == "" || *grokErr != "" {
		return
	}
	*grokErr = msg
}

// parseGrokStreamingEvents reads streaming-json lines, streams text chunks to
// onChunk, accumulates the final assistant text, and records error messages.
// Thought events are ignored for the final text (they are internal reasoning).
// Non-success end stopReasons (e.g. Cancelled) and max_turns_reached set
// grokErr so partial text is not treated as a completed run.
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
	scanner.Buffer(make([]byte, 0, 64*1024), grokScannerMaxTokenSize)

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
		case "max_turns_reached":
			setGrokErrIfEmpty(grokErr, "max turns reached")
		case "end":
			if !grokStopReasonSuccess(event.StopReason) {
				setGrokErrIfEmpty(grokErr, "stopReason="+strings.TrimSpace(event.StopReason))
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
func parseGrokJSONStdout(ctx context.Context, r io.Reader, text *string, structured *json.RawMessage, grokErr *string) error {
	raw, readErr := io.ReadAll(r)
	if err := ctx.Err(); err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}

	parsedText, parsedStructured, parsedErr, err := parseGrokJSONResult(raw)
	if err != nil {
		return err
	}
	if text != nil {
		*text = parsedText
	}
	if structured != nil {
		*structured = parsedStructured
	}
	if grokErr != nil {
		*grokErr = parsedErr
	}
	return nil
}

// parseGrokJSONResult decodes a headless --output-format json payload.
// Success: {"text":"...","stopReason":"EndTurn"} and, with --json-schema,
// optionally {"structuredOutput":{...}}. Failure: {"type":"error","message":"..."},
// non-EndTurn stopReason (e.g. Cancelled / max turns), or a structuredOutputError
// with no usable structured or text payload.
// text is always the envelope's text field; structuredOutput is returned
// separately so finalizeGrokResult can mirror Claude's native field path.
func parseGrokJSONResult(raw []byte) (text string, structured json.RawMessage, grokErr string, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", nil, "", nil
	}

	var envelope struct {
		Type                  string          `json:"type"`
		Message               string          `json:"message"`
		Text                  string          `json:"text"`
		StopReason            string          `json:"stopReason"`
		StructuredOutput      json.RawMessage `json:"structuredOutput"`
		StructuredOutputError string          `json:"structuredOutputError"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return "", nil, "", fmt.Errorf("decode json result: %w", err)
	}
	structured = bytes.TrimSpace(envelope.StructuredOutput)
	hasStructured := len(structured) > 0 && !bytes.Equal(structured, []byte("null"))
	if !hasStructured {
		structured = nil
	}
	soErr := strings.TrimSpace(envelope.StructuredOutputError)
	if envelope.Type == "error" || (envelope.Message != "" && envelope.Text == "" && !hasStructured && envelope.StopReason == "") {
		return "", nil, firstNonEmpty(envelope.Message, soErr, "unknown error"), nil
	}
	if !grokStopReasonSuccess(envelope.StopReason) {
		msg := "stopReason=" + strings.TrimSpace(envelope.StopReason)
		if soErr != "" {
			msg = msg + ": " + soErr
		}
		return envelope.Text, structured, msg, nil
	}
	if soErr != "" && envelope.Text == "" && !hasStructured {
		return "", nil, soErr, nil
	}
	return envelope.Text, structured, "", nil
}
