package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// copilotAgent spawns the GitHub Copilot CLI for each invocation. Copilot
// runs non-interactively with `copilot -p <prompt> --output-format json`,
// emitting JSONL events on stdout. The lifecycle is codex/pi-shaped: one
// process per Run, no managed server.
type copilotAgent struct {
	bin       string
	extraArgs []string
}

func (a *copilotAgent) Name() string { return "copilot" }

func (a *copilotAgent) ReportsAgentAttempts() bool { return true }

func (a *copilotAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "copilot", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *copilotAgent) Close() error { return nil }

func (a *copilotAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	prompt := buildCopilotPrompt(opts.Prompt, opts.JSONSchema)
	args := a.buildArgs(prompt)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("copilot start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "copilot", pid)

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	var usage TokenUsage
	var messages []string
	var copilotErr string
	exitCode := 0
	if err := parseCopilotEvents(ctx, started.stdout, opts.OnChunk, &usage, &messages, &copilotErr, &exitCode); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		retErr := fmt.Errorf("copilot parse events: %w", err)
		emitAgentExited(opts, "copilot", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()

	detail := copilotErrorDetail(copilotErr, string(stderrBuf))
	if waitErr != nil {
		if detail != "" {
			retErr := fmt.Errorf("copilot exited: %w: %s", waitErr, detail)
			emitAgentExited(opts, "copilot", pid, retErr)
			return nil, retErr
		}
		retErr := fmt.Errorf("copilot exited: %w", waitErr)
		emitAgentExited(opts, "copilot", pid, retErr)
		return nil, retErr
	}
	if exitCode != 0 {
		if detail != "" {
			retErr := fmt.Errorf("copilot reported exit code %d: %s", exitCode, detail)
			emitAgentExited(opts, "copilot", pid, retErr)
			return nil, retErr
		}
		retErr := fmt.Errorf("copilot reported exit code %d", exitCode)
		emitAgentExited(opts, "copilot", pid, retErr)
		return nil, retErr
	}

	res, err := finalizeCopilotResult(messages, opts.JSONSchema, usage)
	emitAgentExited(opts, "copilot", pid, err)
	return res, err
}

// finalizeCopilotResult converts the assistant messages emitted during a run
// into a structured Result. With no schema it uses the final message verbatim.
// With a schema it tries each assistant message newest-first and returns the
// first that parses against it: Copilot is non-deterministic about honoring the
// JSON-only output contract and frequently emits the schema JSON in an earlier
// message, then closes with a prose summary (e.g. "Now I've applied all four
// fixes…") that no extraction strategy can recover. If none parse it falls back
// to the final message so the returned error reflects the actual final output.
func finalizeCopilotResult(messages []string, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	lastMessage := ""
	if len(messages) > 0 {
		lastMessage = messages[len(messages)-1]
	}
	if len(schema) == 0 {
		return finalizeTextResult("copilot", lastMessage, schema, usage)
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if result, err := finalizeTextResult("copilot", messages[i], schema, usage); err == nil {
			return result, nil
		}
	}
	return finalizeTextResult("copilot", lastMessage, schema, usage)
}

func copilotErrorDetail(copilotErr, stderr string) string {
	detail := strings.TrimSpace(copilotErr)
	stderr = strings.TrimSpace(stderr)
	if detail != "" && stderr != "" {
		return detail + "; " + stderr
	}
	if detail != "" {
		return detail
	}
	return stderr
}

// buildArgs constructs the copilot CLI arguments. User-supplied extraArgs
// (from agent_args_override) are inserted ahead of the managed flags so user
// choices (e.g. --model, --effort) win over no-mistakes' defaults. If the user
// supplied their own permission flag, the default --allow-all-tools is not
// added; --no-ask-user is always added so the agent never blocks waiting for
// interactive input.
func (a *copilotAgent) buildArgs(prompt string) []string {
	args := make([]string, 0, len(a.extraArgs)+8)
	args = append(args, a.extraArgs...)
	args = append(args,
		"-p", prompt,
		"--output-format", "json",
		"--no-color",
	)
	if !copilotUserSetAskUser(a.extraArgs) {
		args = append(args, "--no-ask-user")
	}
	if !copilotUserSetPermissionMode(a.extraArgs) {
		args = append(args, "--allow-all-tools")
	}
	return args
}

// copilotUserSetPermissionMode reports whether extraArgs already grant tool
// auto-approval, in which case buildArgs skips its default --allow-all-tools.
// Only a blanket approval flag (--allow-all-tools, --allow-all, --yolo) or an
// explicit allowlist (--allow-tool) counts. Flags that merely restrict the tool
// set or filesystem paths (--available-tools, --excluded-tools, --deny-tool,
// --allow-all-paths) do not grant approval, so the non-interactive -p run still
// needs the default --allow-all-tools to avoid blocking on approval prompts.
func copilotUserSetPermissionMode(extraArgs []string) bool {
	for _, arg := range extraArgs {
		switch {
		case arg == "--allow-all-tools",
			arg == "--allow-all",
			arg == "--yolo",
			arg == "--allow-tool":
			return true
		case strings.HasPrefix(arg, "--allow-tool="):
			return true
		}
	}
	return false
}

// copilotUserSetAskUser reports whether extraArgs already control the ask_user
// tool, in which case buildArgs skips its default --no-ask-user.
func copilotUserSetAskUser(extraArgs []string) bool {
	for _, arg := range extraArgs {
		if arg == "--no-ask-user" {
			return true
		}
	}
	return false
}

// buildCopilotPrompt appends a JSON-output contract to the user prompt when a
// schema is provided. The Copilot CLI has no equivalent of codex's
// --output-schema flag, so we inline the schema in the prompt the same way pi
// and rovodev do, then parse the final text with finalizeTextResult.
func buildCopilotPrompt(prompt string, schema json.RawMessage) string {
	if len(schema) == 0 {
		return prompt
	}
	pretty, err := json.MarshalIndent(json.RawMessage(schema), "", "  ")
	if err != nil {
		pretty = []byte(schema)
	}
	return prompt + "\n\n## no-mistakes final output contract\n\n" +
		"When the task is complete, your final assistant response must be only valid JSON matching this JSON Schema. " +
		"Do not wrap it in Markdown fences. Do not include prose before or after the JSON object.\n\n" +
		string(pretty)
}

// copilotEvent is the top-level JSONL event from the copilot CLI.
type copilotEvent struct {
	Type     string            `json:"type"`
	Data     *copilotEventData `json:"data,omitempty"`
	ExitCode *int              `json:"exitCode,omitempty"`
}

type copilotEventData struct {
	// assistant.message_delta
	DeltaContent string `json:"deltaContent,omitempty"`
	// assistant.message
	Content      string `json:"content,omitempty"`
	OutputTokens int    `json:"outputTokens,omitempty"`
	// error / abort events
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// parseCopilotEvents reads JSONL from the reader and dispatches events. It
// streams assistant.message_delta content to onChunk, appends each non-empty
// assistant.message text to messages (oldest first), accumulates output tokens,
// and records the terminal result event's exit code.
func parseCopilotEvents(
	ctx context.Context,
	r io.Reader,
	onChunk func(string),
	usage *TokenUsage,
	messages *[]string,
	copilotErr *string,
	exitCode *int,
) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), claudeScannerMaxTokenSize)

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

		var event copilotEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue // skip malformed lines
		}

		switch event.Type {
		case "assistant.message_delta":
			if event.Data != nil && event.Data.DeltaContent != "" && onChunk != nil {
				onChunk(event.Data.DeltaContent)
			}

		case "assistant.message":
			if event.Data == nil {
				continue
			}
			usage.Add(TokenUsage{OutputTokens: event.Data.OutputTokens, Reported: true})
			if event.Data.Content != "" && messages != nil {
				*messages = append(*messages, event.Data.Content)
			}

		case "error", "assistant.abort":
			if event.Data != nil && copilotErr != nil {
				if msg := firstNonEmpty(event.Data.Message, event.Data.Error); msg != "" {
					*copilotErr = msg
				}
			}

		case "result":
			if event.ExitCode != nil && exitCode != nil {
				*exitCode = *event.ExitCode
			}
		}
	}

	return scanner.Err()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
