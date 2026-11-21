package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ompMaxRetries is the number of additional attempts past the initial one.
const ompMaxRetries = 3

// ompAgent spawns the native omp (Oh My Pi) CLI for each invocation.
//
// The launch shape is verified against omp 19.0.1: `--mode json` makes omp
// non-interactive and emits the pi-lineage JSONL event stream on stdout, the
// prompt is read from stdin, and `--model` / `--thinking` set the model and
// reasoning depth. Structured output is prompt-inlined and validated after the
// turn because omp exposes no schema-constrained output tool no-mistakes can
// verify.
//
// Sessions are deliberately cold: no-mistakes does not resume omp sessions.
// Two pipeline turns never share conversational state, which also removes the
// historical reviewer/fixer session split.
type ompAgent struct {
	bin                    string
	extraArgs              []string
	disableProjectSettings bool
	overlayPath            string
	subprocessContext
	closeOnce sync.Once
}

func (a *ompAgent) Name() string { return string(types.AgentOMP) }

func (a *ompAgent) ReportsAgentAttempts() bool { return true }

// NeutralizesGateInstructions reports whether omp is launched with the target
// repository's project instructions suppressed. The suppression flags are
// appended after operator args, and the gate overlay is the last --config
// layer, so neither an operator override nor the target repo's own
// .omp/config.yml can re-enable project context.
func (a *ompAgent) NeutralizesGateInstructions() bool {
	return a.disableProjectSettings && a.overlayPath != ""
}

func (a *ompAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "omp", opts, ompMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *ompAgent) Close() error {
	a.closeOnce.Do(func() {
		if a.overlayPath != "" {
			_ = os.Remove(a.overlayPath)
		}
	})
	return nil
}

// buildArgs returns the omp argv for one invocation. Operator/mapped flags
// come first, followed by the managed JSON-mode and cold-session flags. Under
// the trusted project-settings opt-out, the suppression flags and overlay come
// last so they outrank an operator/target configuration.
func (a *ompAgent) buildArgs() []string {
	args := make([]string, 0, len(a.extraArgs)+10)
	args = append(args, a.extraArgs...)
	args = append(args, "--mode", "json", "--no-session")
	if a.disableProjectSettings {
		args = append(args, ompGateSuppressionFlags...)
		if a.overlayPath != "" {
			args = append(args, "--config", a.overlayPath)
		}
	}
	return args
}

func (a *ompAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	args := a.buildArgs()
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Env = a.gitSafeEnv(opts.CWD, opts.Env)
	shellenv.ConfigureShellCommand(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("omp stdin pipe: %w", err)
	}

	started, err := startNativeAgentCommand(cmd, nativeAgentActivityObserver(opts, "omp"), opts.RequireCompletedChildren)
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("omp start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "omp", pid)

	prompt := inlineSchemaPrompt(opts.Prompt, opts.JSONSchema)
	stdinErrCh := writeNativeAgentStdin(stdin, prompt)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	pp := &ompEventParser{onChunk: opts.OnChunk}
	if err := pp.parse(ctx, started.stdout); err != nil {
		err = started.waitAfterParseError(err)
		if !started.closedByWaitDelay(err) || !pp.agentEnded {
			stderrWG.Wait()
			err = errors.Join(err, ompStdinError(<-stdinErrCh))
			retErr := fmt.Errorf("omp parse events (result received: %t): %w", pp.agentEnded, err)
			emitAgentExited(opts, "omp", pid, retErr)
			return pp.failedResult(), retErr
		}
	}

	waitErr := started.waitKeepingResult(pp.agentEnded)
	stderrWG.Wait()
	stdinErr := ompStdinError(<-stdinErrCh)
	stderr := strings.TrimSpace(string(stderrBuf))
	if waitErr != nil {
		if stderr != "" {
			retErr := fmt.Errorf("omp exited: %w: %s", errors.Join(waitErr, stdinErr), stderr)
			emitAgentExited(opts, "omp", pid, retErr)
			return pp.failedResult(), retErr
		}
		retErr := fmt.Errorf("omp exited: %w", errors.Join(waitErr, stdinErr))
		emitAgentExited(opts, "omp", pid, retErr)
		return pp.failedResult(), retErr
	}
	if stdinErr != nil {
		if stderr != "" {
			stdinErr = fmt.Errorf("%w: %s", stdinErr, stderr)
		}
		emitAgentExited(opts, "omp", pid, stdinErr)
		return pp.failedResult(), stdinErr
	}

	if pp.assistantError != "" {
		retErr := fmt.Errorf("omp reported error: %s", pp.assistantError)
		emitAgentExited(opts, "omp", pid, retErr)
		return pp.failedResult(), retErr
	}

	res, err := finalizeTextResult("omp", pp.finalText(), opts.JSONSchema, pp.usage)
	if res == nil {
		res = pp.failedResult()
	}
	if res != nil {
		res.Model = pp.model
		res.ModelProvider = pp.provider
	}
	emitAgentExited(opts, "omp", pid, err)
	return res, err
}

func ompStdinError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("omp stdin: %w", err)
}
