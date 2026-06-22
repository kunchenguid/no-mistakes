package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// grokMaxRetries is the number of additional attempts past the initial
// invocation. With 2 retries the agent makes up to 3 total attempts before
// surfacing a transient API error to the pipeline.
const grokMaxRetries = 2

// grokAgent spawns the grok CLI for each invocation.
type grokAgent struct {
	bin       string
	extraArgs []string
}

func (a *grokAgent) Name() string { return "grok" }

func (a *grokAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "grok", opts, grokMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *grokAgent) Close() error { return nil }

func (a *grokAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	prompt := buildGrokPrompt(opts.Prompt, opts.JSONSchema)

	f, err := os.CreateTemp("", "nm-grok-*.md")
	if err != nil {
		return nil, fmt.Errorf("grok prompt temp file: %w", err)
	}
	promptPath := f.Name()
	defer os.Remove(promptPath)

	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("grok prompt temp file write: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("grok prompt temp file close: %w", err)
	}

	args := buildGrokArgs(promptPath, a.extraArgs, opts.CWD)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil
	cmd.Env = gitSafeEnv(opts.CWD)
	// Run in a dedicated process group so cancelling ctx reaps the grok CLI and
	// any subprocesses it spawns, not just the direct child.
	shellenv.ConfigureShellCommand(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("grok stdout pipe: %w", err)
	}

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrR, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("grok stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("grok start: %w", err)
	}

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(stderrR)
	}()

	var textBuf strings.Builder
	// Stream stdout line-by-line with a bufio.Reader instead of bufio.Scanner:
	// the scanner requires a fixed max-token buffer, and a single very long line
	// (e.g. minified JSON) would force a large up-front allocation. ReadString
	// grows incrementally with no fixed cap.
	reader := bufio.NewReader(stdout)
	for {
		select {
		case <-ctx.Done():
			stderrWG.Wait()
			_ = cmd.Wait()
			return nil, ctx.Err()
		default:
		}
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			if textBuf.Len() > 0 {
				textBuf.WriteByte('\n')
				if opts.OnChunk != nil {
					opts.OnChunk("\n")
				}
			}
			textBuf.WriteString(line)
			if opts.OnChunk != nil {
				opts.OnChunk(line)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			stderrWG.Wait()
			_ = cmd.Wait()
			return nil, fmt.Errorf("grok read stdout: %w", readErr)
		}
	}

	stderrWG.Wait()
	if err := cmd.Wait(); err != nil {
		stderr := strings.TrimSpace(string(stderrBuf))
		if stderr != "" {
			return nil, fmt.Errorf("grok exited: %w: %s", err, stderr)
		}
		return nil, fmt.Errorf("grok exited: %w", err)
	}

	return finalizeTextResult("grok", textBuf.String(), opts.JSONSchema, TokenUsage{})
}

// buildGrokArgs constructs the grok CLI arguments. extraArgs are placed first,
// followed by the managed flags; grok treats the last occurrence of a flag as
// authoritative, so the managed flags take precedence. These flags are reserved
// in config (users cannot supply them), keeping the gate's invocation contract
// intact.
func buildGrokArgs(promptPath string, extraArgs []string, cwd string) []string {
	args := make([]string, 0, len(extraArgs)+8)
	args = append(args, extraArgs...)
	args = append(args,
		"--prompt-file", promptPath,
		"--cwd", cwd,
		"--permission-mode", "bypassPermissions",
		"--output-format", "plain",
	)
	return args
}

// buildGrokPrompt prepends a JSON-output contract to the user prompt when a
// schema is provided. Grok has no structured-output flag, so we inline the
// schema in the prompt and ask for a fenced JSON block.
func buildGrokPrompt(prompt string, schema json.RawMessage) string {
	if len(schema) == 0 {
		return prompt
	}
	pretty, err := json.MarshalIndent(json.RawMessage(schema), "", "  ")
	if err != nil {
		pretty = []byte(schema)
	}
	contract := "## no-mistakes final output contract\n\n" +
		"When the iteration is complete, your final assistant response must include valid JSON matching this JSON Schema, wrapped in a Markdown ```json code fence. " +
		"Do not include prose after the fenced JSON block.\n\n" +
		string(pretty)
	return contract + "\n\n" + prompt
}
