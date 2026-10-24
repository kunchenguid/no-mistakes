package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// claudeMaxRetries is the number of additional attempts past the initial
// invocation. With 3 retries the agent makes up to 4 total attempts before
// surfacing a transient API error to the pipeline.
const claudeMaxRetries = 3

// errNoStructuredOutput is returned when Claude succeeds but omits structured output.
var errNoStructuredOutput = errors.New("claude returned no structured output")

// errClaudeIdleAfterSubmission is returned by parseClaudeEvents when the
// session went quiet after an accepted StructuredOutput submission and never
// wrote its result event.
var errClaudeIdleAfterSubmission = errors.New("claude went idle after its structured submission")

// claudeSubmittedIdleGrace is how long a session may stay quiet, with no
// main-thread tool call in flight, after an accepted StructuredOutput
// submission before it is ended with that submission kept. Claude can promise
// a fuller submission once background work finishes and then never make it,
// which otherwise idles until the invocation budget kills the session and
// discards the submission it did make. A variable so tests can shorten it.
var claudeSubmittedIdleGrace = claudeSubmittedIdleGraceUnder(config.DefaultStepQuietWarning)

// claudeSubmittedIdleGraceUnder derives the idle grace from the pipeline's
// quiet stop, which cancels an invocation past its silent budget once it has
// been quiet that long and so would otherwise discard the submission as a
// timeout. The pipeline measures quiet from activity throttled to
// nativeAgentActivityInterval, so it can count from up to that much before
// the last line; ending the process group is allowed
// claudeIdleEndBeforeDeadline, the same room left ahead of a deadline. The
// grace is shorter than the quiet stop by both, so the idle end finishes first.
func claudeSubmittedIdleGraceUnder(quietStop time.Duration) time.Duration {
	return quietStop - nativeAgentActivityInterval - claudeIdleEndBeforeDeadline
}

// claudeIdleEndBeforeDeadline is how far ahead of the invocation's deadline an
// idle session holding an accepted submission is ended when that deadline
// comes before claudeSubmittedIdleGrace, leaving room to end the process
// group and return before the deadline, since a return after it is a timeout
// whatever it carries. A variable so tests can shorten it.
var claudeIdleEndBeforeDeadline = time.Minute

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
	return fmt.Sprintf("API error status %d: %s", e.Status, string(message))
}

func (e *claudeAPIError) classify() (string, bool) {
	if e.usageLimitReached || isTerminalRetryError(strings.ToLower(e.Message)) {
		return "", false
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
	parseErr := parseClaudeEvents(ctx, started.stdout, opts.OnChunk, &usage, &result)
	idleAfterSubmission := errors.Is(parseErr, errClaudeIdleAfterSubmission)
	if parseErr != nil && !idleAfterSubmission {
		err := started.waitAfterParseError(parseErr)
		stderrWG.Wait()
		retErr := fmt.Errorf("claude parse events: %w", err)
		emitAgentExited(opts, "claude", pid, retErr)
		return claudeFailedResult(opts, usage, result), retErr
	}

	var waitErr error
	if idleAfterSubmission && opts.RequireCompletedChildren {
		// Ending the session kills whatever its process group still runs, so
		// the submission cannot be shown to cover completed work.
		_ = started.waitAfterParseError(parseErr)
		stderrWG.Wait()
		retErr := fmt.Errorf("claude completion is unverified: %w and stayed quiet, so its process group was ended before its owned work could be shown complete", parseErr)
		emitAgentExited(opts, "claude", pid, retErr)
		return resultFromUsage(usage), retErr
	}
	if idleAfterSubmission {
		if opts.OnChunk != nil {
			opts.OnChunk("claude stayed idle after its accepted StructuredOutput submission without finishing its turn; ending the session and keeping that submission")
		}
		// The exit status is the termination this adapter just caused, not
		// the agent's verdict, so it is not reported as a failure.
		_ = started.waitAfterParseError(parseErr)
	} else {
		waitErr = started.wait()
	}
	stderrWG.Wait()
	if waitErr != nil {
		retErr := claudeExitError(waitErr, string(stderrBuf), result.terminalAPIError())
		emitAgentExited(opts, "claude", pid, retErr)
		return claudeFailedResult(opts, usage, result), retErr
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

// claudeFailedResult is what a failed invocation returns: its usage, plus the
// latest accepted submission so the failure never discards what was validly
// submitted. A turn that requires completed children keeps nothing, since the
// session did not end cleanly enough to show its owned work finished.
func claudeFailedResult(opts RunOpts, usage TokenUsage, result *claudeResult) *Result {
	res := resultFromUsage(usage)
	if result == nil || len(result.StructuredOutput) == 0 || opts.RequireCompletedChildren {
		return res
	}
	if res == nil {
		res = &Result{}
	}
	res.Output = result.StructuredOutput
	if opts.OnChunk != nil {
		opts.OnChunk(fmt.Sprintf("claude failed after an accepted StructuredOutput submission; keeping that submission: %s", result.StructuredOutput))
	}
	return res
}

func claudeExitError(waitErr error, stderr string, apiErr *claudeAPIError) error {
	if apiErr == nil {
		return fmt.Errorf("claude exited: %w: %s", waitErr, stderr)
	}
	if strings.TrimSpace(stderr) == "" {
		return fmt.Errorf("claude exited: %w: %w", waitErr, apiErr)
	}
	return fmt.Errorf("claude exited: %w: %s: %w", waitErr, strings.TrimSpace(stderr), apiErr)
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
	}
	if err := json.Unmarshal(r.rawEvent, &fields); err != nil || fields.APIErrorStatus == 0 {
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
	// ParentToolUseID is set on events from a subagent's own conversation.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`

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
// It accumulates token usage and captures the final result event. When the
// stream stays quiet after an accepted StructuredOutput submission, with no
// main-thread tool call in flight and no result event yet, for
// claudeSubmittedIdleGrace or until claudeIdleEndBeforeDeadline ahead of ctx's
// deadline, whichever comes first, it returns errClaudeIdleAfterSubmission so
// the caller ends the session. Whenever it returns without a result event, it
// records the latest accepted submission as the result. Assistant prose never
// stands in for a submission.
func parseClaudeEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, result **claudeResult) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), claudeScannerMaxTokenSize)
	lines := make(chan []byte)
	scanDone := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-stop:
				return
			}
		}
		scanDone <- scanner.Err()
	}()

	p := &claudeEventParser{
		onChunk:            onChunk,
		usage:              usage,
		result:             result,
		usageByMsg:         make(map[string]TokenUsage),
		pendingSubmissions: make(map[string]json.RawMessage),
		inFlightTools:      make(map[string]bool),
	}
	defer p.keepSubmissionAsResult()
	idle := time.NewTimer(claudeSubmittedIdleGrace)
	idle.Stop()
	defer idle.Stop()
	var idleC <-chan time.Time

	for {
		var line []byte
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-scanDone:
			return err
		case <-idleC:
			return errClaudeIdleAfterSubmission
		case line = <-lines:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		p.handle(line)

		idle.Stop()
		idleC = nil
		if p.awaitingOnlyItsResult() {
			idle.Reset(claudeSubmittedIdleWait(ctx))
			idleC = idle.C
		}
	}
}

// claudeSubmittedIdleWait is the quiet period after an accepted submission
// before the session is ended. The margin ahead of ctx's deadline never
// exceeds half the remaining budget, so the wait stays positive and a result
// event arriving right after the submission still finishes the turn.
func claudeSubmittedIdleWait(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return claudeSubmittedIdleGrace
	}
	remaining := time.Until(deadline)
	return min(claudeSubmittedIdleGrace, remaining-min(claudeIdleEndBeforeDeadline, remaining/2))
}

// claudeEventParser holds the state parseClaudeEvents carries across lines.
type claudeEventParser struct {
	onChunk func(string)
	usage   *TokenUsage
	result  **claudeResult

	textBuf           string
	lastSessionID     string
	lastModel         string
	usageLimitReached bool
	usageByMsg        map[string]TokenUsage
	// A terminal result event can carry only prose after background work
	// completes, so the latest accepted StructuredOutput submission is kept
	// and used when the result event has no structured payload of its own.
	pendingSubmissions map[string]json.RawMessage
	lastSubmission     json.RawMessage
	// A main-thread tool call still awaiting its result (a long test run, a
	// foreground subagent) is work in progress however quiet the stream is.
	inFlightTools map[string]bool
	sawResult     bool
}

// awaitingOnlyItsResult reports whether the session has an accepted
// submission and nothing left in flight, so continued quiet means it is not
// going to finish its turn.
func (p *claudeEventParser) awaitingOnlyItsResult() bool {
	return p.lastSubmission != nil && len(p.inFlightTools) == 0 && !p.sawResult
}

func (p *claudeEventParser) keepSubmissionAsResult() {
	if p.result == nil || p.sawResult || p.lastSubmission == nil {
		return
	}
	*p.result = &claudeResult{
		Subtype:          "success",
		StructuredOutput: p.lastSubmission,
		text:             p.textBuf,
		sessionID:        p.lastSessionID,
		model:            p.lastModel,

		usageLimitReached: p.usageLimitReached,
	}
}

func (p *claudeEventParser) handle(line []byte) {
	if len(line) == 0 {
		return
	}

	var event claudeEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return // skip malformed lines
	}
	if event.SessionID != "" {
		p.lastSessionID = event.SessionID
	}
	mainThread := event.ParentToolUseID == ""

	switch event.Type {
	case "assistant":
		var msg claudeMessage
		if err := json.Unmarshal(event.Message, &msg); err != nil {
			return
		}
		if msg.Model != "" {
			p.lastModel = msg.Model
		}
		// Content blocks repeat cumulative usage for the same message.
		p.usageByMsg[msg.ID] = msg.Usage.tokenUsage()
		*p.usage = accumulateUsage(p.usageByMsg)
		for _, c := range msg.Content {
			if c.Type == "text" && c.Text != "" {
				p.textBuf += c.Text
				if p.onChunk != nil {
					p.onChunk(c.Text)
				}
			}
			if c.Type == "tool_use" && c.ID != "" && mainThread {
				p.inFlightTools[c.ID] = true
			}
			if c.Type == "tool_use" && c.Name == claudeStructuredOutputTool && c.ID != "" && len(c.Input) > 0 {
				p.pendingSubmissions[c.ID] = c.Input
			}
		}

	case "user":
		var msg claudeMessage
		if err := json.Unmarshal(event.Message, &msg); err != nil {
			return
		}
		for _, c := range msg.Content {
			if c.Type != "tool_result" {
				continue
			}
			if mainThread {
				delete(p.inFlightTools, c.ToolUseID)
			}
			input, ok := p.pendingSubmissions[c.ToolUseID]
			if !ok {
				continue
			}
			delete(p.pendingSubmissions, c.ToolUseID)
			if !c.IsError {
				p.lastSubmission = input
			}
		}

	case "rate_limit_event":
		if event.RateLimitInfo != nil {
			p.usageLimitReached = event.RateLimitInfo.Status == "rejected"
		}

	case "result":
		p.sawResult = true
		// The invocation total includes usage absent from assistant events.
		if event.Usage != nil {
			*p.usage = event.Usage.tokenUsage()
		}
		if p.result != nil {
			raw := make(json.RawMessage, len(line))
			copy(raw, line)
			structured := event.StructuredOutput
			if len(structured) == 0 || string(structured) == "null" {
				structured = p.lastSubmission
			} else {
				p.lastSubmission = structured
			}
			*p.result = &claudeResult{
				Subtype:          event.Subtype,
				IsError:          event.IsError,
				StructuredOutput: structured,
				text:             p.textBuf,
				rawEvent:         raw,
				sessionID:        p.lastSessionID,
				model:            p.lastModel,

				usageLimitReached: p.usageLimitReached,
			}
		}
	}
}
