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

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// claudeMaxRetries is the number of additional attempts past the initial
// invocation. With 3 retries the agent makes up to 4 total attempts before
// surfacing a transient API error to the pipeline.
const claudeMaxRetries = 3

// errNoStructuredOutput is returned when Claude succeeds but omits structured output.
var errNoStructuredOutput = errors.New("claude returned no structured output")

const claudeScannerMaxTokenSize = 256 * 1024 * 1024

// claudeAPIErrorPrefix is how the claude CLI labels its own transport failures
// ("API Error: Stream idle timeout - no chunks received"). The CLI writes them
// as assistant text on the stdout event stream, not to stderr.
const claudeAPIErrorPrefix = "API Error:"

// claudeAPIErrorLimit bounds the diagnostics lifted out of the stream, which
// end up in a step error and therefore in runs.error.
const claudeAPIErrorLimit = 512

// claudeStream captures the assistant text stream so a non-zero exit can report
// why claude stopped. Without it the exit error is built from an empty stderr,
// which both hides the cause from the user and leaves classifyTransient nothing
// to recognize, so a recoverable stall fails the run instead of retrying.
// It keeps only the CLI's own API Error diagnostics, deduplicated and bounded.
// It deliberately keeps nothing else: the rest of the stream is model output
// about the user's code, and these errors are persisted to runs.error.
type claudeStream struct {
	found []string
	seen  map[string]bool
	size  int
}

// tee returns a chunk callback that scans the stream and forwards to onChunk.
func (s *claudeStream) tee(onChunk func(string)) func(string) {
	return func(chunk string) {
		s.observe(chunk)
		if onChunk != nil {
			onChunk(chunk)
		}
	}
}

// observe records the API Error diagnostics in one assistant text block, line
// by line: the CLI writes each diagnostic as its own line.
func (s *claudeStream) observe(chunk string) {
	for line := range strings.SplitSeq(chunk, "\n") {
		if s.size >= claudeAPIErrorLimit {
			return
		}
		s.observeLine(strings.TrimRight(line, "\r"))
	}
}

// observeLine records the diagnostics on a single line. Only a line that
// STARTS with the CLI's own prefix counts: an unanchored search also captures
// model prose that merely mentions the literal, and these strings are then
// persisted to runs.error and presented as why claude stopped. Because the CLI
// emits a diagnostic without a guaranteed trailing newline, a second prefix
// later in the same line opens the next diagnostic and closes this one.
func (s *claudeStream) observeLine(line string) {
	rest := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(rest, claudeAPIErrorPrefix) {
		return
	}
	for s.size < claudeAPIErrorLimit {
		body := rest[len(claudeAPIErrorPrefix):]
		end := len(body)
		if next := strings.Index(body, claudeAPIErrorPrefix); next >= 0 {
			end = next
		}
		s.add(claudeAPIErrorPrefix + strings.TrimRight(body[:end], " \t"))
		if end == len(body) {
			return
		}
		rest = body[end:]
	}
}

func (s *claudeStream) add(line string) {
	if s.seen == nil {
		s.seen = make(map[string]bool)
	}
	if s.seen[line] {
		return
	}
	s.seen[line] = true
	s.found = append(s.found, line)
	s.size += len(line)
}

// apiErrors renders the retained diagnostics as a single bounded detail string.
func (s *claudeStream) apiErrors() string {
	joined := strings.Join(s.found, "; ")
	if len(joined) > claudeAPIErrorLimit {
		joined = joined[:claudeAPIErrorLimit]
	}
	return joined
}

// claudeExitError explains a non-zero claude exit from both channels: stderr
// leads, and the CLI diagnostics carried on the event stream are appended
// rather than used only as a fallback. Anything at all on stderr - a warning,
// a deprecation notice - would otherwise hide the stall detail, which is the
// only text classifyTransient can recognize to spend a retry on.
func claudeExitError(waitErr error, stderr []byte, stream *claudeStream) error {
	if detail := claudeFailureDetail(stderr, stream); detail != "" {
		return fmt.Errorf("claude exited: %w: %s", waitErr, detail)
	}
	return fmt.Errorf("claude exited: %w", waitErr)
}

// claudeFailureDetail joins whatever each channel reported, skipping a stream
// diagnostic stderr already carries so the error never says it twice.
func claudeFailureDetail(stderr []byte, stream *claudeStream) string {
	var details []string
	errText := strings.TrimSpace(string(stderr))
	if errText != "" {
		details = append(details, errText)
	}
	if detail := stream.apiErrors(); detail != "" && !strings.Contains(errText, detail) {
		details = append(details, detail)
	}
	return strings.Join(details, "; ")
}

// withClaudeStreamDetail appends the stream's CLI diagnostics to msg when there
// are any, so a bare failure never reads as an unexplained one.
func withClaudeStreamDetail(msg string, stream *claudeStream) string {
	if detail := stream.apiErrors(); detail != "" {
		return msg + ": " + detail
	}
	return msg
}

// claudeAgent spawns the claude CLI for each invocation.
type claudeAgent struct {
	bin       string
	extraArgs []string
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
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	started, err := startNativeAgentCommand(cmd)
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
	var stream claudeStream
	if err := parseClaudeEvents(ctx, started.stdout, stream.tee(opts.OnChunk), &usage, &result); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		// Reading the event stream fails on cancellation, a read error, or an
		// event past the scanner's token limit - never on a stream that simply
		// stops, which bufio hands back as a final token. Whatever the cause,
		// the diagnostics the CLI already put on the stream explain it, so this
		// path carries them exactly like the wait-error and no-result paths.
		retErr := fmt.Errorf("claude parse events: %w", err)
		if detail := claudeFailureDetail(stderrBuf, &stream); detail != "" {
			retErr = fmt.Errorf("claude parse events: %w: %s", err, detail)
		}
		emitAgentExited(opts, "claude", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		retErr := claudeExitError(waitErr, stderrBuf, &stream)
		emitAgentExited(opts, "claude", pid, retErr)
		return nil, retErr
	}

	if result == nil {
		retErr := errors.New(withClaudeStreamDetail("claude returned no result event", &stream))
		emitAgentExited(opts, "claude", pid, retErr)
		return nil, retErr
	}

	res, err := finalizeClaudeResult(result, opts.JSONSchema, usage)
	if res != nil {
		res.SessionID = result.sessionID
		res.Resumed = resumeID != ""
		res.Model = result.model
		res.SkillsUsed = result.skillsUsed
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

func finalizeClaudeResult(result *claudeResult, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	if result.IsError || result.Subtype != "success" {
		return nil, fmt.Errorf("claude error: subtype=%s", result.Subtype)
	}
	if len(schema) > 0 && result.StructuredOutput == nil {
		return nil, errNoStructuredOutput
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
	// skillsUsed lists, in call order, the skill names the turn invoked via
	// the Skill tool. Non-nil (possibly empty) whenever the stream parsed,
	// so callers can tell "invoked nothing" from "adapter reports nothing".
	skillsUsed []string
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
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// parseClaudeEvents reads JSONL from the reader and dispatches events.
// It accumulates token usage and captures the final result event.
func parseClaudeEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, result **claudeResult) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), claudeScannerMaxTokenSize)
	var textBuf string
	var lastSessionID string
	var lastModel string
	skillsUsed := []string{}

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
				InputTokens:           msg.Usage.InputTokens,
				OutputTokens:          msg.Usage.OutputTokens,
				CacheReadTokens:       msg.Usage.CacheReadInputTokens,
				CacheCreationTokens:   msg.Usage.CacheCreationInputTokens,
				Reported:              true,
				CacheCreationReported: true,
			})
			for _, c := range msg.Content {
				if c.Type == "text" && c.Text != "" {
					textBuf += c.Text
					if onChunk != nil {
						onChunk(c.Text)
					}
				}
				if c.Type == "tool_use" && c.Name == "Skill" {
					var in struct {
						Skill string `json:"skill"`
					}
					if err := json.Unmarshal(c.Input, &in); err == nil && in.Skill != "" {
						skillsUsed = append(skillsUsed, in.Skill)
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
					text:             textBuf,
					rawEvent:         raw,
					sessionID:        lastSessionID,
					model:            lastModel,
					skillsUsed:       skillsUsed,
				}
			}
		}
	}

	return scanner.Err()
}
