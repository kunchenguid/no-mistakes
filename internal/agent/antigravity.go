package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// antigravityAgent spawns the agy CLI for each invocation.
type antigravityAgent struct {
	subprocessContext
	bin       string
	extraArgs []string
}

func (a *antigravityAgent) Name() string { return "antigravity" }

func (a *antigravityAgent) ReportsAgentAttempts() bool { return true }

// SupportsSessionResume reports antigravity's durable-session capability:
// stream-json events carry the conversation identity, and
// `--conversation <id>` reopens that conversation headless.
func (a *antigravityAgent) SupportsSessionResume() bool { return true }

// SupportsSessionProvider accepts sessions minted under either spelling of
// the provider name, so a session recorded before a config rename still
// resumes.
func (a *antigravityAgent) SupportsSessionProvider(provider string) bool {
	return provider == "antigravity" || provider == "agy"
}

func (a *antigravityAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "antigravity", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *antigravityAgent) Close() error { return nil }

func (a *antigravityAgent) buildArgs(prompt, schemaPath, sessionID string) []string {
	// Antigravity has strict flag parsing: only --print, --json-schema, --output-format
	// We append user extraArgs before the strict ones.
	args := make([]string, 0, len(a.extraArgs)+11)
	args = append(args, a.extraArgs...)
	if sessionID != "" {
		args = append(args, "--conversation", sessionID)
	}
	args = append(args, "--dangerously-skip-permissions")
	if !antigravityUserSetPrintTimeout(a.extraArgs) {
		args = append(args, "--print-timeout", "24h")
	}
	args = append(args, "--print", prompt)
	if schemaPath != "" {
		args = append(args, "--json-schema", schemaPath)
	}
	args = append(args, "--output-format", "stream-json")
	return args
}

// antigravityUserSetPrintTimeout reports whether extraArgs already specifies
// --print-timeout, in which case buildArgs does not override it.
func antigravityUserSetPrintTimeout(extraArgs []string) bool {
	for i, arg := range extraArgs {
		if arg == "--print-timeout" || strings.HasPrefix(arg, "--print-timeout=") {
			return true
		}
		if strings.HasPrefix(arg, "-t=") || (arg == "-t" && i+1 < len(extraArgs)) {
			return true
		}
	}
	return false
}

func (a *antigravityAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	schemaPath := ""
	if len(opts.JSONSchema) > 0 {
		f, err := os.CreateTemp("", "no-mistakes-antigravity-schema-*.json")
		if err != nil {
			return nil, fmt.Errorf("antigravity schema temp file: %w", err)
		}
		schemaPath = f.Name()
		if _, err := f.Write(opts.JSONSchema); err != nil {
			_ = f.Close()
			_ = os.Remove(schemaPath)
			return nil, fmt.Errorf("antigravity schema temp file write: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(schemaPath)
			return nil, fmt.Errorf("antigravity schema temp file close: %w", err)
		}
		defer os.Remove(schemaPath)
	}

	bin := a.bin
	if bin == "" {
		bin = "agy"
	}
	requestedSession := ""
	if opts.Session != nil {
		requestedSession = opts.Session.ID
	}
	args := a.buildArgs(opts.Prompt, schemaPath, requestedSession)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = opts.CWD
	cmd.Env = a.gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	started, err := startNativeAgentCommand(cmd, nativeAgentActivityObserver(opts, "antigravity"))
	if err != nil {
		return nil, fmt.Errorf("antigravity start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "antigravity", pid)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	pp := &antigravityParser{onChunk: opts.OnChunk}
	if err := pp.parse(ctx, started.stdout); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		retErr := fmt.Errorf("antigravity parse events: %w", err)
		emitAgentExited(opts, "antigravity", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		stderr := strings.TrimSpace(string(stderrBuf))
		if stderr != "" {
			retErr := fmt.Errorf("antigravity exited: %w: %s", waitErr, stderr)
			emitAgentExited(opts, "antigravity", pid, retErr)
			return nil, retErr
		}
		retErr := fmt.Errorf("antigravity exited: %w", waitErr)
		emitAgentExited(opts, "antigravity", pid, retErr)
		return nil, retErr
	}

	if pp.errorMessage != "" {
		retErr := fmt.Errorf("antigravity reported error: %s", pp.errorMessage)
		emitAgentExited(opts, "antigravity", pid, retErr)
		return nil, retErr
	}

	text := pp.finalText()
	res, err := finalizeTextResult("antigravity", text, opts.JSONSchema, pp.usage)
	if res != nil && pp.sessionID != "" {
		res.SessionID = pp.sessionID
		res.Provider = "antigravity"
		res.Resumed = requestedSession != "" && requestedSession == pp.sessionID
	}
	emitAgentExited(opts, "antigravity", pid, err)
	return res, err
}

type antigravityParser struct {
	onChunk func(string)

	streamText   string
	sessionID    string
	structured   string
	response     string
	usage        TokenUsage
	errorMessage string
}

// Typed view of `agy --output-format stream-json`. One JSON object per line;
// every event names the conversation serving it.
type agyStreamEvent struct {
	Event          string         `json:"event"`
	ConversationID string         `json:"conversation_id"`
	Init           *agyInitData   `json:"init,omitempty"`
	StepUpdate     *agyStepUpdate `json:"step_update,omitempty"`
	Result         *agyResultData `json:"result,omitempty"`
}

type agyInitData struct {
	CWD   string   `json:"cwd"`
	Tools []string `json:"tools"`
}

type agyStepUpdate struct {
	ConversationID string          `json:"conversation_id"`
	StepIndex      int             `json:"step_index"`
	State          string          `json:"state"`
	StepType       string          `json:"step_type"`
	TextDelta      string          `json:"text_delta"`
	ToolCallDelta  string          `json:"tool_call_delta"`
	InputJSONDelta string          `json:"input_json_delta"`
	ArgumentsDelta string          `json:"arguments_delta"`
	ToolCalls      []agyToolCall   `json:"tool_calls,omitempty"`
	ToolInfo       *agyToolInfo    `json:"tool_info,omitempty"`
	SubagentInfo   json.RawMessage `json:"subagent_info,omitempty"`
	DurationSecs   float64         `json:"duration_seconds"`
	Usage          *agyUsageData   `json:"usage,omitempty"`
}

type agyToolCall struct {
	Delta          string          `json:"delta,omitempty"`
	InputJSONDelta string          `json:"input_json_delta,omitempty"`
	ArgumentsDelta string          `json:"arguments_delta,omitempty"`
	Function       *agyFunctionArg `json:"function,omitempty"`
}

type agyFunctionArg struct {
	Arguments string `json:"arguments,omitempty"`
}

// agyToolInfo carries the invoked tool's parameters either as a pre-rendered
// JSON string or as a structured value, so Parameters stays raw until render.
type agyToolInfo struct {
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

type agyResultData struct {
	ConversationID   string          `json:"conversation_id"`
	Status           string          `json:"status"`
	Response         string          `json:"response"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	Error            string          `json:"error,omitempty"`
	DurationSecs     float64         `json:"duration_seconds"`
	NumTurns         int             `json:"num_turns"`
	Usage            *agyUsageData   `json:"usage,omitempty"`
}

// Every counter is a pointer so absence of a key stays distinguishable
// from a provider-reported zero; only keys actually present in a payload
// overwrite the accumulated usage.
type agyUsageData struct {
	InputTokens         *int `json:"input_tokens"`
	OutputTokens        *int `json:"output_tokens"`
	ThinkingTokens      *int `json:"thinking_tokens"`
	CacheReadTokens     *int `json:"cache_read_tokens"`
	CacheCreationTokens *int `json:"cache_creation_tokens"`
	TotalTokens         *int `json:"total_tokens"`
}

// agyPayloadText renders a raw JSON payload for the stream: a JSON string is
// unwrapped verbatim; any other value is compacted in place, preserving the
// field order agy emitted.
func agyPayloadText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", false
	}
	return buf.String(), true
}

func (p *antigravityParser) parse(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)

	var sb strings.Builder

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

		var event agyStreamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			var typeErr *json.UnmarshalTypeError
			if !errors.As(err, &typeErr) {
				continue
			}
			// A field whose shape drifted still leaves the rest of the
			// event populated (encoding/json decodes past type
			// mismatches), so degrade per-field instead of dropping the
			// whole line.
		}

		// init carries the conversation identity at the top level; every
		// event of the run names the conversation actually serving it.
		if event.ConversationID != "" {
			p.sessionID = event.ConversationID
		}

		switch event.Event {
		case "init":
			// Nothing beyond the top-level conversation identity.
		case "step_update":
			step := event.StepUpdate
			if step == nil {
				continue
			}
			if step.ConversationID != "" {
				p.sessionID = step.ConversationID
			}

			var delta strings.Builder
			delta.WriteString(step.TextDelta)
			delta.WriteString(step.ToolCallDelta)
			delta.WriteString(step.InputJSONDelta)
			delta.WriteString(step.ArgumentsDelta)
			for _, tc := range step.ToolCalls {
				delta.WriteString(tc.Delta)
				delta.WriteString(tc.InputJSONDelta)
				delta.WriteString(tc.ArgumentsDelta)
				if tc.Function != nil {
					delta.WriteString(tc.Function.Arguments)
				}
			}
			// Specialized payloads with newline padding.
			if step.ToolInfo != nil {
				if params, ok := agyPayloadText(step.ToolInfo.Parameters); ok {
					delta.WriteString("\n" + params + "\n")
				}
			}
			if sub, ok := agyPayloadText(step.SubagentInfo); ok {
				delta.WriteString("\n" + sub + "\n")
			}

			if d := delta.String(); d != "" {
				sb.WriteString(d)
				if p.onChunk != nil {
					p.onChunk(d)
				}
			}

			if step.Usage != nil {
				applyAgyUsage(&p.usage, step.Usage, false)
			}
		case "result":
			res := event.Result
			if res == nil {
				continue
			}
			if res.ConversationID != "" {
				p.sessionID = res.ConversationID
			}
			if res.Status == "ERROR" {
				if res.Error != "" {
					p.errorMessage = res.Error
				} else {
					p.errorMessage = "unknown error"
				}
			}
			// The terminal answer is authoritative wherever agy puts it:
			// result.response outranks stream deltas even when some were
			// collected, and structured_output outranks both (finalText).
			if res.Response != "" {
				p.response = res.Response
			}
			if trimmed := strings.TrimSpace(string(res.StructuredOutput)); trimmed != "" && trimmed != "null" {
				var buf bytes.Buffer
				if json.Compact(&buf, res.StructuredOutput) == nil {
					p.structured = buf.String()
				}
			}
			if res.Usage != nil {
				applyAgyUsage(&p.usage, res.Usage, true)
			}
		}
	}

	p.streamText = sb.String()
	return scanner.Err()
}

func (p *antigravityParser) finalText() string {
	switch {
	case p.structured != "":
		return strings.TrimSpace(p.structured)
	case p.response != "":
		return strings.TrimSpace(p.response)
	default:
		return strings.TrimSpace(p.streamText)
	}
}

// applyAgyUsage merges an agy usage payload into TokenUsage: each counter
// overwrites only when this payload reports it, so the last reported value
// wins per field and absent keys never regress earlier values to zero.
// Presence of thinking_tokens or cache_creation_tokens, not their values,
// sets the matching Reported flag so genuine zeros stay distinguishable
// from an adapter that never exposes the field. cache_creation_tokens is
// honored only on the terminal result payload, matching the historical
// step_update handling.
func applyAgyUsage(target *TokenUsage, src *agyUsageData, includeCacheCreation bool) {
	if src == nil {
		return
	}
	target.Reported = true
	if src.InputTokens != nil {
		target.InputTokens = *src.InputTokens
	}
	if src.OutputTokens != nil {
		target.OutputTokens = *src.OutputTokens
	}
	if src.CacheReadTokens != nil {
		target.CacheReadTokens = *src.CacheReadTokens
	}
	if includeCacheCreation && src.CacheCreationTokens != nil {
		target.CacheCreationTokens = *src.CacheCreationTokens
		target.CacheCreationReported = true
	}
	if src.ThinkingTokens != nil {
		target.ReasoningReported = true
		target.ReasoningTokens = *src.ThinkingTokens
	}
}
