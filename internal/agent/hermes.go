package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// hermesAgent spawns the Hermes CLI for each invocation. Hermes runs
// non-interactively with `hermes -z <prompt> --yolo`, printing only the final
// response text to stdout. The lifecycle is one process per Run, no managed
// server, no JSONL event stream — Hermes emits plain text, so parsing is a
// single stdout read rather than event-by-event dispatch.
type hermesAgent struct {
	bin       string
	extraArgs []string
}

func (a *hermesAgent) Name() string { return "hermes" }

func (a *hermesAgent) ReportsAgentAttempts() bool { return true }

func (a *hermesAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "hermes", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *hermesAgent) Close() error { return nil }

func (a *hermesAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	prompt := buildHermesPrompt(opts.Prompt, opts.JSONSchema)
	args := a.buildArgs(prompt)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("hermes start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "hermes", pid)

	// Hermes prints plain text to stdout. We read it fully rather than
	// streaming JSONL events like codex/copilot/pi. If an OnChunk callback is
	// registered, forward the captured text after the run completes so the
	// TUI/log pipeline still records the output.
	var stdoutBuf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, readErr := started.stdout.Read(buf)
			if n > 0 {
				stdoutBuf.Write(buf[:n])
			}
			if readErr != nil {
				return
			}
		}
	}()

	var stderrBuf []byte
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		// Read stderr in chunks; nativeAgentPipe.Close() ends this.
		buf := make([]byte, 4096)
		for {
			n, readErr := started.stderr.Read(buf)
			if n > 0 {
				stderrBuf = append(stderrBuf, buf[:n]...)
			}
			if readErr != nil {
				return
			}
		}
	}()

	waitErr := started.wait()
	<-done
	<-stderrDone

	text := stdoutBuf.String()
	if opts.OnChunk != nil && text != "" {
		emitHermesChunks(opts.OnChunk, text)
	}

	if waitErr != nil {
		stderr := strings.TrimSpace(string(stderrBuf))
		var retErr error
		if stderr != "" {
			retErr = fmt.Errorf("hermes exited: %w: %s", waitErr, stderr)
		} else {
			retErr = fmt.Errorf("hermes exited: %w", waitErr)
		}
		emitAgentExited(opts, "hermes", pid, retErr)
		return nil, retErr
	}

	res, err := finalizeTextResult("hermes", text, opts.JSONSchema, TokenUsage{})
	emitAgentExited(opts, "hermes", pid, err)
	return res, err
}

// buildArgs constructs the Hermes CLI arguments. User-supplied extraArgs
// (from agent_args_override) are inserted ahead of the managed flags so user
// choices (e.g. --model, --provider) win over no-mistakes' defaults.
//
// --ignore-rules is always added: no-mistakes needs Hermes as a clean-slate
// code agent, not the user's full SOUL.md/router/skills personality. Without
// it, Hermes may route a code-review prompt to a weather skill or delegation
// profile and return non-JSON output that fails schema parsing.
//
// --ignore-user-config is NOT added because it also suppresses provider/model
// configuration and credential resolution, causing Hermes to fail with "No
// inference provider configured" or auth errors. --ignore-rules alone is
// sufficient to suppress SOUL.md/AGENTS.md routing without breaking auth.
func (a *hermesAgent) buildArgs(prompt string) []string {
	args := make([]string, 0, len(a.extraArgs)+5)
	args = append(args, a.extraArgs...)
	args = append(args,
		"-z", prompt,
		"--ignore-rules",
	)
	if !hermesUserSetApproval(a.extraArgs) {
		args = append(args, "--yolo")
	}
	return args
}

// hermesUserSetApproval reports whether extraArgs already grant auto-approval,
// in which case buildArgs skips its default --yolo.
func hermesUserSetApproval(extraArgs []string) bool {
	for _, arg := range extraArgs {
		switch {
		case arg == "--yolo":
			return true
		case arg == "--safe-mode":
			return true
		case strings.HasPrefix(arg, "--safe-mode="):
			return true
		}
	}
	return false
}

// buildHermesPrompt appends a JSON-output contract to the user prompt when a
// schema is provided. The Hermes CLI has no structured-output flag, so we
// inline the schema in the prompt the same way pi, copilot, and rovodev do,
// then parse the final text with finalizeTextResult.
func buildHermesPrompt(prompt string, schema json.RawMessage) string {
	if len(schema) == 0 {
		return prompt
	}
	pretty, err := json.MarshalIndent(json.RawMessage(schema), "", "  ")
	if err != nil {
		pretty = []byte(schema)
	}
	return prompt + "\n\n## no-mistakes final output contract\n\n" +
		"When the task is complete, your final response must be only valid JSON matching this JSON Schema. " +
		"Do not wrap it in Markdown fences. Do not include prose before or after the JSON object.\n\n" +
		string(pretty)
}

// emitHermesChunks forwards the captured plain-text output to the streaming
// callback. Since Hermes does not emit incremental events, the text arrives as
// a single block after process exit; we deliver it in bounded chunks so very
// large outputs do not produce one giant callback payload.
func emitHermesChunks(onChunk func(string), text string) {
	const chunkSize = 8192
	for len(text) > chunkSize {
		onChunk(text[:chunkSize])
		text = text[chunkSize:]
	}
	if text != "" {
		onChunk(text)
	}
}
