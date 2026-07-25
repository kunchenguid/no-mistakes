package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// ompAgent spawns the Oh My Pi (omp) CLI for each invocation. OMP is a Pi
// fork: it emits the same JSONL event stream on stdout under --mode json, so
// the streaming parser and structured-output contract are shared with the Pi
// backend (piParser, buildPiPrompt). Unlike Pi, OMP ignores stdin under -p, so
// the prompt is written to a temp file and passed as an `@<file>` message
// argument - this avoids the Linux MAX_ARG_STRLEN (128 KiB) per-argument cap
// that a large positional prompt (review instructions + full branch diff)
// would hit as E2BIG. The lifecycle is codex-shaped: one process per Run.
type ompAgent struct {
	bin       string
	extraArgs []string
}

func (a *ompAgent) Name() string { return "omp" }

func (a *ompAgent) ReportsAgentAttempts() bool { return true }

func (a *ompAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "omp", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *ompAgent) Close() error { return nil }

func (a *ompAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	prompt := buildPiPrompt(opts.Prompt, opts.JSONSchema)
	// OMP ignores stdin under -p, and a single positional argv element is capped
	// at 128 KiB on Linux (E2BIG on large diffs), so deliver the prompt through
	// OMP's `@<file>` message mechanism: the argv carries only a short path.
	promptFile, err := os.CreateTemp("", "omp-gate-prompt-*.md")
	if err != nil {
		return nil, fmt.Errorf("omp prompt file: %w", err)
	}
	defer os.Remove(promptFile.Name())
	if _, err := promptFile.WriteString(prompt); err != nil {
		promptFile.Close()
		return nil, fmt.Errorf("omp prompt file: %w", err)
	}
	if err := promptFile.Close(); err != nil {
		return nil, fmt.Errorf("omp prompt file: %w", err)
	}
	cmd := exec.CommandContext(ctx, a.bin, a.buildArgs(promptFile.Name())...)
	cmd.Dir = opts.CWD
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("omp start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "omp", pid)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	pp := &piParser{onChunk: opts.OnChunk}
	if err := pp.parse(ctx, started.stdout); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		retErr := fmt.Errorf("omp parse events: %w", err)
		emitAgentExited(opts, "omp", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		stderr := strings.TrimSpace(string(stderrBuf))
		if stderr != "" {
			retErr := fmt.Errorf("omp exited: %w: %s", waitErr, stderr)
			emitAgentExited(opts, "omp", pid, retErr)
			return nil, retErr
		}
		retErr := fmt.Errorf("omp exited: %w", waitErr)
		emitAgentExited(opts, "omp", pid, retErr)
		return nil, retErr
	}

	if pp.assistantError != "" {
		retErr := fmt.Errorf("omp reported error: %s", pp.assistantError)
		emitAgentExited(opts, "omp", pid, retErr)
		return nil, retErr
	}

	text := pp.finalText()
	res, err := finalizeTextResult("omp", text, opts.JSONSchema, pp.usage)
	emitAgentExited(opts, "omp", pid, err)
	return res, err
}

// buildArgs returns the OMP argv for one invocation. User extras come first
// (so user --model/--provider take effect), then the managed flags that
// no-mistakes requires, and finally the prompt file as an `@<file>` message
// argument. OMP ignores stdin under -p, and a single argv element is capped at
// 128 KiB on Linux, so the prompt is delivered by file reference, not inline.
//
// --no-extensions, --no-skills, and --no-rules stop OMP from auto-discovering
// the contributor worktree's project extensions, skills, and rules (OMP runs
// with cmd.Dir set to that untrusted tree). This is best-effort: OMP - like the
// claude and pi agents - still discovers project MCP servers, hooks, and
// commands from the worktree's .omp/ and .claude/, so a pushed branch can still
// inject config into the fix-agent. Fully isolating every fix-agent from
// untrusted-worktree config is a separate, no-mistakes-wide hardening. Explicit
// user -e paths in extraArgs still load.
func (a *ompAgent) buildArgs(promptFile string) []string {
	args := make([]string, 0, len(a.extraArgs)+8)
	args = append(args, a.extraArgs...)
	args = append(args, "-p", "--mode", "json", "--no-session", "--no-extensions", "--no-skills", "--no-rules", "@"+promptFile)
	return args
}
