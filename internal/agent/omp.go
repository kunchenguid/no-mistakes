package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ompAgent spawns the omp CLI for each invocation. omp is a fork of Pi that
// reads its prompt from stdin and emits JSONL on stdout when --mode json is
// set. The lifecycle is identical to piAgent: one process per Run, no managed
// server.
type ompAgent struct {
	bin       string
	extraArgs []string
}

func (a *ompAgent) Name() string { return "omp" }

func (a *ompAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "omp", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *ompAgent) Close() error { return nil }

func (a *ompAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	args := a.buildArgs()
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("omp stdin pipe: %w", err)
	}

	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("omp start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "omp", pid)

	prompt := buildOMPPrompt(opts.Prompt, opts.JSONSchema)
	go func() {
		defer stdin.Close()
		_, _ = io.WriteString(stdin, prompt)
	}()

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

// buildArgs returns the omp argv for one invocation. User extras come first
// (so user --provider/--model take effect), then the managed flags that
// no-mistakes requires for JSONL parsing.
func (a *ompAgent) buildArgs() []string {
	args := make([]string, 0, len(a.extraArgs)+4)
	args = append(args, a.extraArgs...)
	args = append(args, "--mode", "json", "--no-session")
	return args
}

// buildOMPPrompt appends a JSON-output contract to the user prompt when a
// schema is provided. omp has no --output-schema flag, so we inline the
// schema in the prompt the same way we do for Pi.
func buildOMPPrompt(prompt string, schema json.RawMessage) string {
	if len(schema) == 0 {
		return prompt
	}
	pretty, err := json.MarshalIndent(json.RawMessage(schema), "", "  ")
	if err != nil {
		pretty = []byte(schema)
	}
	return prompt + "\n\n## no-mistakes final output contract\n\n" +
		"When the iteration is complete, your final assistant response must be only valid JSON matching this JSON Schema. " +
		"Do not wrap it in Markdown fences. Do not include prose before or after the JSON object.\n\n" +
		string(pretty)
}

// compile-time check that ompAgent implements Agent.
var _ Agent = (*ompAgent)(nil)

// ensure types import is used.
var _ types.AgentName = types.AgentOMP
