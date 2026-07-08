package agent

import (
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

// geminiAgent spawns the gemini CLI for each invocation.
type geminiAgent struct {
	bin       string
	extraArgs []string
}

func (a *geminiAgent) Name() string { return "gemini" }

func (a *geminiAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "gemini", opts, claudeMaxRetries, claudeRetryClassifier, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *geminiAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
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
		return nil, fmt.Errorf("gemini start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "gemini", pid)

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
		retErr := fmt.Errorf("gemini parse events: %w", err)
		emitAgentExited(opts, "gemini", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		retErr := fmt.Errorf("gemini exited: %w: %s", waitErr, string(stderrBuf))
		emitAgentExited(opts, "gemini", pid, retErr)
		return nil, retErr
	}

	if result == nil {
		retErr := fmt.Errorf("gemini returned no result event")
		emitAgentExited(opts, "gemini", pid, retErr)
		return nil, retErr
	}

	res, err := finalizeClaudeResult(result, opts.JSONSchema, usage)
	if errors.Is(err, errNoStructuredOutput) && opts.OnChunk != nil {
		opts.OnChunk(fmt.Sprintf("structured output missing: subtype=%s, text_len=%d, input_tokens=%d, output_tokens=%d",
			result.Subtype, len(result.text), usage.InputTokens, usage.OutputTokens))
		opts.OnChunk(fmt.Sprintf("raw result event: %s", string(result.rawEvent)))
	}
	emitAgentExited(opts, "gemini", pid, err)
	return res, err
}

func (a *geminiAgent) Close() error { return nil }

func (a *geminiAgent) buildArgs(prompt string, schema json.RawMessage) []string {
	args := make([]string, 0, len(a.extraArgs)+10)
	args = append(args, a.extraArgs...)
	args = append(args,
		"-p", prompt,
		"--verbose",
		"--output-format", "stream-json",
	)
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	}
	if !geminiUserSetModel(a.extraArgs) {
		args = append(args, "--model", "gemini-3.1-pro")
	}
	if !geminiUserSetPermissionMode(a.extraArgs) {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

func geminiUserSetModel(extraArgs []string) bool {
	for _, arg := range extraArgs {
		if arg == "--model" || strings.HasPrefix(arg, "--model=") || arg == "-m" || strings.HasPrefix(arg, "-m=") {
			return true
		}
	}
	return false
}

func geminiUserSetPermissionMode(extraArgs []string) bool {
	for _, arg := range extraArgs {
		if arg == "--dangerously-skip-permissions" ||
			arg == "--permission-mode" ||
			strings.HasPrefix(arg, "--permission-mode=") {
			return true
		}
	}
	return false
}
