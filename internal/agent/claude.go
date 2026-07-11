package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// claudeMaxRetries is the number of additional attempts past the initial
// invocation. With 3 retries the agent makes up to 4 total attempts before
// surfacing a transient API error to the pipeline.
const claudeMaxRetries = 3

// errNoStructuredOutput is returned when Claude succeeds but omits structured output.
var errNoStructuredOutput = errors.New("claude returned no structured output")

const claudeScannerMaxTokenSize = 256 * 1024 * 1024

// claudeAgent spawns the claude CLI for each invocation.
type claudeAgent struct {
	bin       string
	extraArgs []string
}

func (a *claudeAgent) Name() string { return "claude" }

// SupportsSessionResume reports claude's native durable-session capability:
// every stream-json event carries a session_id, and `claude -p --resume <id>`
// continues that session in print mode with the same identity.
func (a *claudeAgent) SupportsSessionResume() bool { return true }

func (a *claudeAgent) ReportsAgentAttempts() bool { return true }

func (a *claudeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	res, err := runWithRetry(ctx, "claude", opts, claudeMaxRetries, claudeRetryClassifier, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
	return res, withOperationalClassification(ctx, err)
}

func (a *claudeAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	resumeID := ""
	if opts.Session != nil {
		resumeID = opts.Session.ID
	}
	args := a.buildArgs(opts)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil
	cmd.Env = gitSafeEnv(opts.CWD)
	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	started, err := startNativeProcess(cmd)
	if err != nil {
		return nil, fmt.Errorf("claude start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "claude", pid)

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	var usage TokenUsage
	var result *claudeResult
	if err := parseClaudeEvents(ctx, started.stdout, opts.OnChunk, &usage, &result); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		retErr := fmt.Errorf("claude parse events: %w", err)
		emitAgentExited(opts, "claude", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		retErr := fmt.Errorf("claude exited: %w: %s", waitErr, string(stderrBuf))
		emitAgentExited(opts, "claude", pid, retErr)
		return nil, retErr
	}

	if result == nil {
		retErr := fmt.Errorf("claude returned no result event")
		emitAgentExited(opts, "claude", pid, retErr)
		return nil, retErr
	}

	res, err := finalizeClaudeResult(result, opts.JSONSchema, usage)
	if res != nil {
		res.SessionID = result.sessionID
		res.Resumed = resumeID != ""
		res.Model = result.model
	}
	if errors.Is(err, errNoStructuredOutput) && opts.OnChunk != nil {
		opts.OnChunk(fmt.Sprintf("structured output missing: subtype=%s, text_len=%d, input_tokens=%d, output_tokens=%d",
			result.Subtype, len(result.text), usage.InputTokens, usage.OutputTokens))
		opts.OnChunk(fmt.Sprintf("raw result event: %s", string(result.rawEvent)))
	}
	emitAgentExited(opts, "claude", pid, err)
	return res, err
}

func (a *claudeAgent) Close() error { return nil }

func finalizeClaudeResult(result *claudeResult, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	if result.IsError || result.Subtype != "success" {
		if detail := claudeResultErrorDetail(result); detail != "" {
			return nil, fmt.Errorf("claude error: subtype=%s: %s", result.Subtype, detail)
		}
		return nil, fmt.Errorf("claude error: subtype=%s", result.Subtype)
	}
	if len(schema) > 0 {
		if result.StructuredOutput == nil {
			return nil, markModelOutput(errNoStructuredOutput)
		}
		if err := validateStructuredOutput(result.StructuredOutput, schema); err != nil {
			return nil, markModelOutput(fmt.Errorf("claude structured output: %w", err))
		}
	}

	return &Result{
		Output: result.StructuredOutput,
		Text:   result.text,
		Usage:  usage,
	}, nil
}

// claudeResultErrorDetail returns provider error detail from a failed result
// event so retry and operational classification can see status codes and
// overload/quota markers. It uses only explicit provider fields — the status
// code and result text — never the raw event JSON, so an incidental number in
// an unrelated field (e.g. duration_ms) can never be mistaken for a provider
// status. Returns "" when the result carries no explicit provider detail.
func claudeResultErrorDetail(result *claudeResult) string {
	parts := make([]string, 0, 2)
	if result.apiErrorStatus != 0 {
		parts = append(parts, fmt.Sprintf("api_error_status=%d", result.apiErrorStatus))
	}
	if detail := strings.TrimSpace(result.resultText); detail != "" {
		parts = append(parts, detail)
	}
	return strings.Join(parts, " ")
}

// buildArgs constructs the claude CLI arguments. User-supplied extraArgs
// (from agent_args_override in the global config) are inserted ahead of the
// managed flags, so user choices win over no-mistakes' defaults. If the user
// supplied their own permission mode, the default --dangerously-skip-permissions
// is not added. A non-empty Session ID continues that session via --resume
// (never --fork-session: the session identity must stay stable so later
// turns keep resuming the same conversation).
func (a *claudeAgent) buildArgs(opts RunOpts) []string {
	args := make([]string, 0, len(a.extraArgs)+12)
	args = append(args, routedClaudeExtraArgs(a.extraArgs, opts.Model != "", opts.Effort != "")...)
	args = append(args,
		"-p", opts.Prompt,
		"--verbose",
		"--output-format", "stream-json",
	)
	if opts.Session != nil && opts.Session.ID != "" {
		args = append(args, "--resume", opts.Session.ID)
	}
	if len(opts.JSONSchema) > 0 {
		args = append(args, "--json-schema", string(opts.JSONSchema))
	}
	// Translate the normalized routed model and effort to claude's native
	// arguments. Empty values (legacy invocations) emit nothing.
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", string(opts.Effort))
	}
	if !claudeUserSetPermissionMode(a.extraArgs) {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

// routedClaudeExtraArgs drops user-supplied model or effort flags when the
// routed invocation supplies those values, so routed model/effort win over a
// legacy override without emitting a duplicate flag. Non-conflicting extras are
// preserved in order.
func routedClaudeExtraArgs(extra []string, hasModel, hasEffort bool) []string {
	if !hasModel && !hasEffort {
		return extra
	}
	out := make([]string, 0, len(extra))
	for i := 0; i < len(extra); i++ {
		arg := extra[i]
		switch {
		case hasModel && (arg == "--model" || arg == "-m"):
			i++
			continue
		case hasModel && (strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "-m=")):
			continue
		case hasEffort && arg == "--effort":
			i++
			continue
		case hasEffort && strings.HasPrefix(arg, "--effort="):
			continue
		}
		out = append(out, arg)
	}
	return out
}

// claudeUserSetPermissionMode reports whether extraArgs already declare a
// permission flag, in which case buildArgs skips its default.
func claudeUserSetPermissionMode(extraArgs []string) bool {
	for _, arg := range extraArgs {
		if arg == "--dangerously-skip-permissions" ||
			arg == "--permission-mode" ||
			strings.HasPrefix(arg, "--permission-mode=") {
			return true
		}
	}
	return false
}

// claudeEvent is the top-level JSONL event from claude CLI.
type claudeEvent struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message,omitempty"`
	SessionID string          `json:"session_id,omitempty"`

	// result fields
	Subtype          string          `json:"subtype,omitempty"`
	IsError          bool            `json:"is_error,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	Result           string          `json:"result,omitempty"`
	APIErrorStatus   int             `json:"api_error_status,omitempty"`
	Usage            *claudeUsage    `json:"usage,omitempty"`
}

// claudeResult captures the parsed result event.
type claudeResult struct {
	Subtype          string
	IsError          bool
	StructuredOutput json.RawMessage
	text             string // accumulated text from assistant events
	rawEvent         json.RawMessage
	sessionID        string // durable session identity from the event stream
	model            string // model reported by assistant events
	resultText       string // provider result/error text from the result event
	apiErrorStatus   int    // provider HTTP status from the result event, if any
}

type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type claudeMessage struct {
	Model   string          `json:"model"`
	Usage   claudeUsage     `json:"usage"`
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// parseClaudeEvents reads JSONL from the reader and dispatches events.
// It accumulates token usage and captures the final result event.
func parseClaudeEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, result **claudeResult) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), claudeScannerMaxTokenSize)
	var textBuf string
	var lastSessionID string
	var lastModel string

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

		var event claudeEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue // skip malformed lines
		}
		if event.SessionID != "" {
			lastSessionID = event.SessionID
		}

		switch event.Type {
		case "assistant":
			var msg claudeMessage
			if err := json.Unmarshal(event.Message, &msg); err != nil {
				continue
			}
			if msg.Model != "" {
				lastModel = msg.Model
			}
			usage.Add(TokenUsage{
				InputTokens:         msg.Usage.InputTokens,
				OutputTokens:        msg.Usage.OutputTokens,
				CacheReadTokens:     msg.Usage.CacheReadInputTokens,
				CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
			})
			for _, c := range msg.Content {
				if c.Type == "text" && c.Text != "" {
					textBuf += c.Text
					if onChunk != nil {
						onChunk(c.Text)
					}
				}
			}

		case "result":
			if result != nil {
				raw := make(json.RawMessage, len(line))
				copy(raw, line)
				*result = &claudeResult{
					Subtype:          event.Subtype,
					IsError:          event.IsError,
					StructuredOutput: event.StructuredOutput,
					resultText:       event.Result,
					apiErrorStatus:   event.APIErrorStatus,
					text:             textBuf,
					rawEvent:         raw,
					sessionID:        lastSessionID,
					model:            lastModel,
				}
			}
		}
	}

	return scanner.Err()
}
