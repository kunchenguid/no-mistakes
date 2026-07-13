package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// codexAgent spawns the codex CLI for each invocation.
type codexAgent struct {
	bin       string
	extraArgs []string
	// disableProjectSettings is the resolved, trusted-only opt-out. When true,
	// buildArgs suppresses codex's project-level settings/instructions surface.
	disableProjectSettings bool
}

func (a *codexAgent) Name() string { return "codex" }

// SupportsSessionResume reports codex's native durable-session capability:
// `codex exec --json` emits thread.started with a thread_id, and
// `codex exec resume <id> <prompt>` continues that thread.
func (a *codexAgent) SupportsSessionResume() bool { return true }

func (a *codexAgent) ReportsAgentAttempts() bool { return true }

// NeutralizesGateInstructions reports whether codex is currently launched with
// the target repo's project-level settings/instructions suppressed. It is
// meaningful only under the opt-out (disableProjectSettings): the gate only
// consults it when the repo opted out. It is honest about the EFFECTIVE knob
// value, not merely its presence - codex neutralizes iff the effective codex
// `project_doc_max_bytes` is 0 (buildArgs appends `=0`, or the operator pinned
// `=0` themselves). An operator override that re-enables the project doc
// (`project_doc_max_bytes` > 0) defeats neutralization, so this returns false
// and the gate fails closed rather than running with the captain-identity hazard
// re-enabled. Verified empirically: with the project doc loaded codex adopts the
// AGENTS.md identity; with project_doc_max_bytes=0 (plus --ignore-rules) it does
// not.
func (a *codexAgent) NeutralizesGateInstructions() bool {
	return a.disableProjectSettings && codexEffectiveProjectDocSuppressed(a.extraArgs)
}

func (a *codexAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "codex", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *codexAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	schemaPath := ""
	validationSchema := opts.JSONSchema
	if len(opts.JSONSchema) > 0 {
		f, err := os.CreateTemp("", "no-mistakes-codex-schema-*.json")
		if err != nil {
			return nil, fmt.Errorf("codex schema temp file: %w", err)
		}
		schemaPath = f.Name()
		schema, err := codexOutputSchema(opts.JSONSchema)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(schemaPath)
			return nil, fmt.Errorf("codex schema normalize: %w", err)
		}
		validationSchema = schema
		if _, err := f.Write(schema); err != nil {
			_ = f.Close()
			_ = os.Remove(schemaPath)
			return nil, fmt.Errorf("codex schema temp file write: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(schemaPath)
			return nil, fmt.Errorf("codex schema temp file close: %w", err)
		}
		defer os.Remove(schemaPath)
	}

	resumeID := ""
	if opts.Session != nil {
		resumeID = opts.Session.ID
	}
	args := a.buildArgs(opts.Prompt, schemaPath, resumeID)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("codex start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "codex", pid)

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	var usage TokenUsage
	var lastMessage string
	var codexErr string
	var threadID string
	metrics := newCodexMetricsAccumulator()
	if err := parseCodexEvents(ctx, started.stdout, opts.OnChunk, &usage, &lastMessage, &codexErr, &threadID, metrics); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		retErr := fmt.Errorf("codex parse events: %w", err)
		emitAgentExited(opts, "codex", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		detail := strings.TrimSpace(codexErr)
		stderr := strings.TrimSpace(string(stderrBuf))
		if detail != "" && stderr != "" {
			detail += "; " + stderr
		} else if detail == "" {
			detail = stderr
		}
		retErr := fmt.Errorf("codex exited: %w: %s", waitErr, detail)
		emitAgentExited(opts, "codex", pid, retErr)
		return nil, retErr
	}

	res, err := finalizeTextResult("codex", lastMessage, validationSchema, usage)
	if res != nil {
		res.SessionID = threadID
		res.Resumed = resumeID != ""
		// codex reports usage cumulatively across a resumed thread and does not
		// surface cache-creation cost, so mark both so the pipeline records
		// correct per-round deltas and an honest unknown for cache creation.
		res.SessionUsageCumulative = true
		m := metrics.metrics()
		res.Metrics = &m
		res.Model, res.ModelProvider = resolveCodexModel(threadID, time.Now())
	}
	emitAgentExited(opts, "codex", pid, err)
	return res, err
}

func (a *codexAgent) Close() error { return nil }

// buildArgs constructs the codex CLI arguments. User-supplied extraArgs are
// inserted between "exec" and the prompt so user flags (e.g. -m, --sandbox)
// take effect. If the user declared their own execution-mode flag, the
// default --dangerously-bypass-approvals-and-sandbox is not added.
// A non-empty resumeID routes through `codex exec resume <id> <prompt>`,
// which exposes a narrower flag surface than `codex exec` (no --color, no
// -s/--sandbox as of codex 0.144): unsupported user extraArgs make the
// invocation fail fast and the caller's cold fallback preserves correctness.
func (a *codexAgent) buildArgs(prompt, schemaPath, resumeID string) []string {
	args := make([]string, 0, len(a.extraArgs)+11)
	args = append(args, "exec")
	if resumeID != "" {
		args = append(args, "resume")
	}
	args = append(args, a.extraArgs...)
	if resumeID != "" {
		args = append(args, resumeID)
	}
	args = append(args, prompt, "--json")
	if schemaPath != "" {
		args = append(args, "--output-schema", schemaPath)
	}
	if !codexUserSetExecutionMode(a.extraArgs) {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	if resumeID == "" {
		args = append(args, "--color", "never")
	}
	// Project-settings opt-out (trusted-only; see config.DisableProjectSettings):
	// suppress codex's project-level settings/instructions so the target repo's
	// AGENTS.md cannot install a fleet-captain identity on the gate agent. The
	// full project surface codex loads from the checkout is the project doc
	// (AGENTS.md) plus project execpolicy `.rules`; codex config itself is
	// user-level ($CODEX_HOME), not project. Both knobs are global overrides
	// accepted by `codex exec` AND `codex exec resume`, appended last so they
	// never disturb codex's `[resume] <id> <prompt>` positionals:
	//   - `-c project_doc_max_bytes=0` makes codex read zero bytes of AGENTS.md
	//     (the identity-bearing surface). Skipped only when the operator pinned
	//     their own project_doc_max_bytes (their choice wins; NeutralizesGate-
	//     Instructions then fails closed if that value re-enables the doc).
	//   - `--ignore-rules` drops project (and user) execpolicy `.rules` for full
	//     project-settings coverage. It is functionally redundant under the gate's
	//     --dangerously-bypass-approvals-and-sandbox (which bypasses execpolicy
	//     anyway) but completes the contract and is robust to future sandbox
	//     changes. Skipped only if the operator already passed it.
	// When the repo did not opt out, none of this is added and codex loads
	// AGENTS.md exactly as before (backward-compat for ordinary repos).
	if a.disableProjectSettings {
		if !codexUserSetProjectDocMaxBytes(a.extraArgs) {
			args = append(args, "-c", "project_doc_max_bytes=0")
		}
		if !codexArgsContain(a.extraArgs, "--ignore-rules") {
			args = append(args, "--ignore-rules")
		}
	}
	return args
}

// codexEffectiveProjectDocSuppressed reports whether the EFFECTIVE codex
// project_doc_max_bytes is 0 (AGENTS.md fully suppressed): true when the
// operator did not pin the value (buildArgs appends `=0`) or pinned it to 0
// themselves, and false when the operator pinned a non-zero (or unparseable)
// value that would re-enable the project doc.
func codexEffectiveProjectDocSuppressed(extraArgs []string) bool {
	value, pinned := codexUserProjectDocMaxBytes(extraArgs)
	if !pinned {
		return true // buildArgs appends project_doc_max_bytes=0
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	return err == nil && n == 0
}

// codexUserSetProjectDocMaxBytes reports whether extraArgs pin
// project_doc_max_bytes at all (so buildArgs does not double-set it).
func codexUserSetProjectDocMaxBytes(extraArgs []string) bool {
	_, pinned := codexUserProjectDocMaxBytes(extraArgs)
	return pinned
}

// codexUserProjectDocMaxBytes returns the operator-pinned project_doc_max_bytes
// value (the last occurrence wins) and whether it was pinned at all. It handles
// both the inline `-c project_doc_max_bytes=<v>` token and the split
// `-c <key=value>` form.
func codexUserProjectDocMaxBytes(extraArgs []string) (string, bool) {
	const key = "project_doc_max_bytes"
	value := ""
	pinned := false
	extract := func(tok string) {
		if i := strings.Index(tok, key+"="); i >= 0 {
			value = tok[i+len(key)+1:]
			pinned = true
		}
	}
	for i, arg := range extraArgs {
		if strings.Contains(arg, key+"=") {
			extract(arg)
			continue
		}
		if (arg == "-c" || arg == "--config") && i+1 < len(extraArgs) &&
			strings.Contains(extraArgs[i+1], key+"=") {
			extract(extraArgs[i+1])
		}
	}
	return value, pinned
}

// codexArgsContain reports whether extraArgs already include the exact flag.
func codexArgsContain(extraArgs []string, flag string) bool {
	for _, arg := range extraArgs {
		if arg == flag {
			return true
		}
	}
	return false
}

// codexUserSetExecutionMode reports whether extraArgs already declare an
// execution/sandbox flag that conflicts with the default bypass.
func codexUserSetExecutionMode(extraArgs []string) bool {
	for _, arg := range extraArgs {
		switch {
		case arg == "--dangerously-bypass-approvals-and-sandbox",
			arg == "--ask-for-approval",
			arg == "--sandbox":
			return true
		case strings.HasPrefix(arg, "--ask-for-approval="),
			strings.HasPrefix(arg, "--sandbox="):
			return true
		}
	}
	return false
}

// codexEvent is the top-level JSONL event from codex CLI.
type codexEvent struct {
	Type     string      `json:"type"`
	Item     *codexItem  `json:"item,omitempty"`
	Usage    *codexUsage `json:"usage,omitempty"`
	Message  string      `json:"message,omitempty"`
	ThreadID string      `json:"thread_id,omitempty"`
}

type codexItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Text    string `json:"text"`
	Command string `json:"command"`
}

type codexUsage struct {
	InputTokens         int `json:"input_tokens"`
	CachedInputTokens   int `json:"cached_input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	ReasoningOutputToks int `json:"reasoning_output_tokens"`
}

// parseCodexEvents reads JSONL from the reader and dispatches events.
// It captures the last agent_message text, the durable thread identity, and
// accumulates token usage.
// metrics, when non-nil, accumulates the bounded per-invocation activity
// evidence (round-trips, tool calls + categories, subprocess wait time). It is
// clocked by time.Now as events arrive, so a tool item's started->completed gap
// is its real subprocess wall time.
func parseCodexEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, lastMessage *string, codexErr *string, threadID *string, metrics *codexMetricsAccumulator) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)

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

		var event codexEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue // skip malformed lines
		}

		switch event.Type {
		case "error":
			if event.Message != "" && codexErr != nil {
				*codexErr = event.Message
			}

		case "thread.started":
			if event.ThreadID != "" && threadID != nil {
				*threadID = event.ThreadID
			}

		case "item.started":
			metrics.onItem(event.Type, event.Item, time.Now())

		case "item.completed":
			metrics.onItem(event.Type, event.Item, time.Now())
			if event.Item != nil && event.Item.Type == "agent_message" {
				*lastMessage = event.Item.Text
				if onChunk != nil {
					onChunk(event.Item.Text)
				}
			}

		case "turn.completed":
			if event.Usage != nil {
				usage.Add(TokenUsage{
					InputTokens:     event.Usage.InputTokens,
					OutputTokens:    event.Usage.OutputTokens,
					CacheReadTokens: event.Usage.CachedInputTokens,
					ReasoningTokens: event.Usage.ReasoningOutputToks,
					Reported:        true,
				})
			}
		}
	}

	return scanner.Err()
}

func codexOutputSchema(schema json.RawMessage) ([]byte, error) {
	var value any
	if err := json.Unmarshal(schema, &value); err != nil {
		return nil, err
	}
	addAdditionalPropertiesFalse(value)
	return json.Marshal(value)
}

func addAdditionalPropertiesFalse(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	required := requiredSet(schema)
	if schema["type"] == "object" {
		if _, ok := schema["additionalProperties"]; !ok {
			schema["additionalProperties"] = false
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		if schema["type"] == "object" {
			schema["required"] = names
		}
		for _, name := range names {
			property := properties[name]
			addAdditionalPropertiesFalse(property)
			if !required[name] {
				allowSchemaNull(property)
			}
		}
	}
	if items, ok := schema["items"]; ok {
		addAdditionalPropertiesFalse(items)
	}
}

func requiredSet(schema map[string]any) map[string]bool {
	required := make(map[string]bool)
	items, _ := schema["required"].([]any)
	for _, item := range items {
		name, ok := item.(string)
		if ok {
			required[name] = true
		}
	}
	return required
}

func allowSchemaNull(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	if enum, ok := schema["enum"].([]any); ok && !containsNil(enum) {
		schema["enum"] = append(enum, nil)
	}
	switch typ := schema["type"].(type) {
	case string:
		if typ != "null" {
			schema["type"] = []any{typ, "null"}
		}
	case []any:
		if !containsString(typ, "null") {
			schema["type"] = append(typ, "null")
		}
	}
}

func containsNil(items []any) bool {
	for _, item := range items {
		if item == nil {
			return true
		}
	}
	return false
}

func containsString(items []any, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
