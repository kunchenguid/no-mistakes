package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// claudeMaxRetries is the number of additional attempts past the initial
// invocation. With 3 retries the agent makes up to 4 total attempts before
// surfacing a transient API error to the pipeline.
const claudeMaxRetries = 3

// errNoStructuredOutput is returned when Claude succeeds but omits structured output.
var errNoStructuredOutput = errors.New("claude returned no structured output")

const claudeScannerMaxTokenSize = 256 * 1024 * 1024

// claudeAPIErrorMessageMaxRunes bounds the CLI's API error text in a step
// failure message.
const claudeAPIErrorMessageMaxRunes = 400

// claudeAPIError is the API error an invocation ended on, read from the
// structured fields of the CLI's result event. The CLI writes that event once,
// after its own retries, so an API error it recovered from never appears here.
// The retry decision reads these fields and never the assistant text, which
// can quote a status code or an error phrase of its own.
type claudeAPIError struct {
	Status  int
	Message string
	// usageLimitReached records that the CLI's last rate_limit_event rejected
	// the request. Such a 429 clears at the limit's reset time, not after a
	// backoff.
	usageLimitReached bool
}

func (e *claudeAPIError) UsageLimitReached() bool {
	return e.usageLimitReached
}

func (e *claudeAPIError) TerminalProviderError() bool {
	_, retry := e.classify()
	return !retry
}

func (e *claudeAPIError) Error() string {
	message := []rune(e.Message)
	if len(message) > claudeAPIErrorMessageMaxRunes {
		message = append(message[:claudeAPIErrorMessageMaxRunes], []rune("...")...)
	}
	if e.Status == 0 {
		return "API error: " + string(message)
	}
	return fmt.Sprintf("API error status %d: %s", e.Status, string(message))
}

func (e *claudeAPIError) classify() (string, bool) {
	if e.usageLimitReached || isTerminalRetryError(strings.ToLower(e.Message)) {
		return "", false
	}
	if e.Status == 0 {
		// Network failures have no HTTP status; classify their terminal
		// diagnostic without letting text override a reported HTTP status.
		return classifyTransient(errors.New(e.Message))
	}
	if status := transientStatusRE.FindString(strconv.Itoa(e.Status)); status != "" {
		return "http " + status, true
	}
	return "", false
}

// claudeAgent spawns the claude CLI for each invocation.
type claudeAgent struct {
	bin       string
	extraArgs []string
	subprocessContext
	// disableProjectSettings is the resolved, trusted-only opt-out. When true,
	// buildArgs suppresses claude's project-level settings/memory surface.
	disableProjectSettings bool
}

func (a *claudeAgent) Name() string { return "claude" }

// SupportsSessionResume reports claude's native durable-session capability:
// every stream-json event carries a session_id, and `claude -p --resume <id>`
// continues that session in print mode with the same identity.
func (a *claudeAgent) SupportsSessionResume() bool { return true }

func (a *claudeAgent) ReportsAgentAttempts() bool { return true }

// NeutralizesGateInstructions reports whether claude is currently launched with
// the target repo's project-level settings/memory suppressed. It is meaningful
// only under the opt-out (disableProjectSettings): the gate only consults it
// when the repo opted out. It is honest about the EFFECTIVE setting sources -
// claude's project surface (project CLAUDE.md/AGENTS.md, .claude/settings.json,
// and .claude/settings.local.json) is dropped iff the effective
// --setting-sources excludes both `project` and `local`. buildArgs appends
// `--setting-sources user` when the operator did not pin their own; an operator
// override that re-adds `project`/`local` defeats neutralization, so this
// returns false and the gate fails closed. Verified empirically: with project
// memory loaded claude adopts the firstmate identity; with --setting-sources
// user it does not.
func (a *claudeAgent) NeutralizesGateInstructions() bool {
	return a.disableProjectSettings && claudeEffectiveSettingSourcesNeutral(a.extraArgs)
}

func (a *claudeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "claude", opts, claudeMaxRetries, claudeRetryClassifier, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *claudeAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	resumeID := ""
	if opts.Session != nil {
		resumeID = opts.Session.ID
	}
	args := a.buildArgs(opts.JSONSchema, resumeID)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	// Claude Code print mode documents text stdin as its non-interactive
	// prompt transport. Giving os/exec an in-memory reader keeps user prompt
	// bytes out of argv and lets Cmd own the bounded concurrent copy, including
	// EOF, early-child-exit, cancellation, and WaitDelay cleanup paths.
	cmd.Stdin = strings.NewReader(opts.Prompt)
	cmd.Env = a.gitSafeEnv(opts.CWD, opts.Env)
	shellenv.ConfigureShellCommand(cmd)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	started, err := startNativeAgentCommand(cmd, nativeAgentActivityObserver(opts, "claude"), opts.RequireCompletedChildren)
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
	var stdoutTail claudeStdoutTail
	if err := parseClaudeEvents(ctx, io.TeeReader(started.stdout, &stdoutTail), opts.OnChunk, &usage, &result); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		retErr := fmt.Errorf("claude parse events: %w", err)
		emitAgentExited(opts, "claude", pid, retErr)
		return resultFromUsage(usage), retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		if ctx.Err() != nil || opts.RequireCompletedChildren || !claudeFinishedWithOutput(result, opts.JSONSchema) {
			retErr := claudeExitError(waitErr, string(stderrBuf), result.terminalAPIError(), stdoutTail.evidence(result))
			emitAgentExited(opts, "claude", pid, retErr)
			return resultFromUsage(usage), retErr
		}
		if opts.OnChunk != nil {
			opts.OnChunk(fmt.Sprintf("claude exited after completing its result: %v; using the result", waitErr))
		}
	}

	if result == nil {
		retErr := fmt.Errorf("claude returned no result event")
		emitAgentExited(opts, "claude", pid, retErr)
		return resultFromUsage(usage), retErr
	}

	res, err := finalizeClaudeResult(result, opts.JSONSchema, usage)
	if res != nil {
		res.SessionID = result.sessionID
		res.Resumed = resumeID != ""
		res.Model = result.model
		// Claude reports cache-creation cost per message, so the accumulated
		// value is meaningful (recorded as a real number, not unknown). Its
		// stream-json usage is per-invocation, not cumulative across --resume,
		// so SessionUsageCumulative stays false and per-round deltas equal the
		// raw counters.
		res.CacheCreationReported = res.UsageReported
		if result.model != "" {
			res.ModelProvider = "anthropic"
		}
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

func claudeExitError(waitErr error, stderr string, apiErr *claudeAPIError, evidence string) error {
	if apiErr == nil {
		if strings.TrimSpace(stderr) == "" {
			return fmt.Errorf("claude exited: %w: %s", waitErr, evidence)
		}
		return fmt.Errorf("claude exited: %w: %s", waitErr, stderr)
	}
	if strings.TrimSpace(stderr) == "" {
		return fmt.Errorf("claude exited: %w: %w", waitErr, apiErr)
	}
	return fmt.Errorf("claude exited: %w: %s: %w", waitErr, strings.TrimSpace(stderr), apiErr)
}

// claudeFinishedWithOutput reports whether the invocation produced a successful
// result event carrying the output the caller asked for, so a nonzero exit
// after it loses nothing.
func claudeFinishedWithOutput(result *claudeResult, schema json.RawMessage) bool {
	if result == nil || result.IsError || result.Subtype != "success" {
		return false
	}
	if len(schema) > 0 {
		return result.StructuredOutput != nil
	}
	return result.text != ""
}

const (
	claudeStdoutTailLines    = 3
	claudeStdoutTailLineRune = 300
	claudeNoOutputEvidence   = "no stderr or stdout output"
)

// claudeStdoutTail keeps the last few non-event lines the CLI wrote to stdout.
// The usage-limit banner and similar CLI messages arrive there as plain text
// between stream-json events, and an exit with empty stderr has no other
// evidence of its cause.
type claudeStdoutTail struct {
	partial []byte
	// skipping drops the rest of a line that outgrew the buffer.
	skipping bool
	lines    []string
}

func (t *claudeStdoutTail) Write(p []byte) (int, error) {
	for rest := p; len(rest) > 0; {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			if !t.skipping {
				t.partial = append(t.partial, rest...)
				// A stream event line can be megabytes long.
				if len(t.partial) > 4*claudeStdoutTailLineRune {
					t.partial, t.skipping = t.partial[:4*claudeStdoutTailLineRune], true
				}
			}
			break
		}
		if t.skipping {
			t.add(string(t.partial))
		} else {
			t.add(string(t.partial) + string(rest[:i]))
		}
		t.partial, t.skipping = nil, false
		rest = rest[i+1:]
	}
	return len(p), nil
}

func (t *claudeStdoutTail) add(line string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "{") {
		return
	}
	if r := []rune(line); len(r) > claudeStdoutTailLineRune {
		line = string(r[:claudeStdoutTailLineRune]) + "..."
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > claudeStdoutTailLines {
		t.lines = t.lines[1:]
	}
}

// evidence describes why an invocation with empty stderr exited nonzero: the
// plain-text stdout tail, else the result event's own subtype, else an explicit
// marker.
func (t *claudeStdoutTail) evidence(result *claudeResult) string {
	t.add(string(t.partial))
	t.partial = nil
	if len(t.lines) > 0 {
		return "stdout: " + strings.Join(t.lines, " | ")
	}
	if result != nil && result.Subtype != "" {
		return "result subtype=" + result.Subtype
	}
	return claudeNoOutputEvidence
}

// terminalAPIError returns the API error the result event reports, or nil when
// the invocation did not end on one. The fields are decoded apart from
// claudeEvent so a change of their wire type costs only this diagnostic and
// never the result event itself.
func (r *claudeResult) terminalAPIError() *claudeAPIError {
	if r == nil || !r.IsError {
		return nil
	}
	var fields struct {
		APIErrorStatus int    `json:"api_error_status"`
		Result         string `json:"result"`
		TerminalReason string `json:"terminal_reason"`
	}
	if err := json.Unmarshal(r.rawEvent, &fields); err != nil {
		return nil
	}
	if fields.APIErrorStatus == 0 && fields.TerminalReason != "api_error" {
		return nil
	}
	return &claudeAPIError{
		Status:            fields.APIErrorStatus,
		Message:           strings.Join(strings.Fields(fields.Result), " "),
		usageLimitReached: r.usageLimitReached,
	}
}

func finalizeClaudeResult(result *claudeResult, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	if result.IsError || result.Subtype != "success" {
		return resultFromUsage(usage), fmt.Errorf("claude error: subtype=%s", result.Subtype)
	}
	if len(schema) > 0 && result.StructuredOutput == nil {
		return resultFromUsage(usage), RejectStructuredOutput(errNoStructuredOutput)
	}

	return &Result{
		Output:                result.StructuredOutput,
		Text:                  result.text,
		Usage:                 usage,
		UsageReported:         usage.Reported,
		CacheCreationReported: usage.CacheCreationReported,
	}, nil
}

// buildArgs constructs the claude CLI arguments. User-supplied extraArgs
// (from agent_args_override in the global config) are inserted ahead of the
// managed flags, so user choices win over no-mistakes' defaults. If the user
// supplied their own permission mode, the default --dangerously-skip-permissions
// is not added. A non-empty resumeID continues that session via --resume
// (never --fork-session: the session identity must stay stable so later
// turns keep resuming the same conversation).
func (a *claudeAgent) buildArgs(schema json.RawMessage, resumeID string) []string {
	args := make([]string, 0, len(a.extraArgs)+11)
	args = append(args, a.extraArgs...)
	args = append(args,
		"-p",
		"--verbose",
		"--output-format", "stream-json",
	)
	// Project-settings opt-out (trusted-only; see config.DisableProjectSettings):
	// load only user-level settings and memory, never the target repo's
	// project/local CLAUDE.md/AGENTS.md, .claude/settings.json, or
	// .claude/settings.local.json. In an agent-orchestration target (firstmate)
	// the project memory otherwise installs a fleet-captain identity on the gate
	// agent; `--setting-sources user` drops the project and local sources (the
	// full project surface) while preserving the operator's own user-level config
	// and auth. Suppressed only when the operator did not pin their own
	// --setting-sources. When the repo did not opt out, nothing is added and
	// claude loads its project memory exactly as before (backward-compat).
	if a.disableProjectSettings && !claudeUserSetSettingSources(a.extraArgs) {
		args = append(args, "--setting-sources", "user")
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	}
	if !claudeUserSetPermissionMode(a.extraArgs) {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

// claudeUserSetSettingSources reports whether extraArgs pin --setting-sources at
// all, in which case buildArgs does not add its own.
func claudeUserSetSettingSources(extraArgs []string) bool {
	_, pinned := claudeUserSettingSources(extraArgs)
	return pinned
}

// claudeUserSettingSources returns the operator-pinned --setting-sources value
// (last occurrence wins) and whether it was pinned. Handles `--setting-sources
// <v>` and `--setting-sources=<v>`.
func claudeUserSettingSources(extraArgs []string) (string, bool) {
	value := ""
	pinned := false
	for i, arg := range extraArgs {
		if arg == "--setting-sources" && i+1 < len(extraArgs) {
			value = extraArgs[i+1]
			pinned = true
		} else if strings.HasPrefix(arg, "--setting-sources=") {
			value = strings.TrimPrefix(arg, "--setting-sources=")
			pinned = true
		}
	}
	return value, pinned
}

// claudeEffectiveSettingSourcesNeutral reports whether the EFFECTIVE claude
// setting sources drop the target repo's project and local surface: true when
// the operator did not pin --setting-sources (buildArgs appends `user`) or
// pinned a value that contains neither `project` nor `local`, and false when the
// operator's value re-adds `project`/`local`.
func claudeEffectiveSettingSourcesNeutral(extraArgs []string) bool {
	value, pinned := claudeUserSettingSources(extraArgs)
	if !pinned {
		return true // buildArgs appends --setting-sources user
	}
	for _, src := range strings.Split(value, ",") {
		switch strings.ToLower(strings.TrimSpace(src)) {
		case "project", "local":
			return false
		}
	}
	return true
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
	Usage            *claudeUsage    `json:"usage,omitempty"`

	// rate_limit_event fields
	RateLimitInfo *claudeRateLimitInfo `json:"rate_limit_info,omitempty"`
}

type claudeRateLimitInfo struct {
	Status string `json:"status"`
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
	// usageLimitReached is true when the last rate_limit_event before the
	// result rejected the request.
	usageLimitReached bool
}

type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u claudeUsage) tokenUsage() TokenUsage {
	return TokenUsage{
		InputTokens:           u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens:          u.OutputTokens,
		CacheReadTokens:       u.CacheReadInputTokens,
		CacheCreationTokens:   u.CacheCreationInputTokens,
		Reported:              true,
		CacheCreationReported: true,
	}
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Usage   claudeUsage     `json:"usage"`
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// tool_use fields
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result fields
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// claudeStructuredOutputTool is the tool Claude Code exposes for submitting
// the --json-schema payload. A run can call it several times; the latest
// accepted call is the review's current state.
const claudeStructuredOutputTool = "StructuredOutput"

// parseClaudeEvents reads JSONL from the reader and dispatches events.
// It accumulates token usage and captures the final result event.
func parseClaudeEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, result **claudeResult) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), claudeScannerMaxTokenSize)
	var textBuf string
	var lastSessionID string
	var lastModel string
	var usageLimitReached bool
	usageByMsg := make(map[string]TokenUsage)
	// A terminal result event can carry only prose after background work
	// completes, so the latest accepted StructuredOutput submission is kept
	// and used when the result event has no structured payload of its own.
	pendingSubmissions := make(map[string]json.RawMessage)
	var lastSubmission json.RawMessage

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
			// Content blocks repeat cumulative usage for the same message.
			usageByMsg[msg.ID] = msg.Usage.tokenUsage()
			*usage = accumulateUsage(usageByMsg)
			for _, c := range msg.Content {
				if c.Type == "text" && c.Text != "" {
					textBuf += c.Text
					if onChunk != nil {
						onChunk(c.Text)
					}
				}
				if c.Type == "tool_use" && c.Name == claudeStructuredOutputTool && c.ID != "" && len(c.Input) > 0 {
					pendingSubmissions[c.ID] = c.Input
				}
			}

		case "user":
			var msg claudeMessage
			if err := json.Unmarshal(event.Message, &msg); err != nil {
				continue
			}
			for _, c := range msg.Content {
				if c.Type != "tool_result" {
					continue
				}
				input, ok := pendingSubmissions[c.ToolUseID]
				if !ok {
					continue
				}
				delete(pendingSubmissions, c.ToolUseID)
				if !c.IsError {
					lastSubmission = input
				}
			}

		case "rate_limit_event":
			if event.RateLimitInfo != nil {
				usageLimitReached = event.RateLimitInfo.Status == "rejected"
			}

		case "result":
			// The invocation total includes usage absent from assistant events.
			if event.Usage != nil {
				*usage = event.Usage.tokenUsage()
			}
			if result != nil {
				raw := make(json.RawMessage, len(line))
				copy(raw, line)
				structured := event.StructuredOutput
				if len(structured) == 0 || string(structured) == "null" {
					structured = lastSubmission
				} else {
					lastSubmission = structured
				}
				*result = &claudeResult{
					Subtype:          event.Subtype,
					IsError:          event.IsError,
					StructuredOutput: structured,
					text:             textBuf,
					rawEvent:         raw,
					sessionID:        lastSessionID,
					model:            lastModel,

					usageLimitReached: usageLimitReached,
				}
			}
		}
	}

	return scanner.Err()
}
