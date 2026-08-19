package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// grokIsolationEnvKeys are firstmate/session identity variables that a live
// Grok TUI stamps into its environment. A child grok that inherits them
// attaches to that session's project even when --cwd points at the invocation
// checkout. Unset them on every spawn.
var grokIsolationEnvKeys = []string{
	"GROK_AGENT",
	"GROK_SESSION_ID",
	"GROK_WORKSPACE_ROOT",
}

// grokAgent spawns the Grok Build TUI CLI (`grok`) for each invocation.
// The lifecycle is pi/copilot-shaped: one process per Run, no managed server.
//
// Isolation is hard. A live firstmate grok session will steal the project
// unless each spawn pins:
//   - --cwd = opts.CWD (the invocation checkout, never firstmate)
//   - a unique --leader-socket under the process temp dir (never
//     ~/.grok/leader.sock)
//   - GROK_AGENT / GROK_SESSION_ID / GROK_WORKSPACE_ROOT stripped from the
//     child env
//
// Headless invoke uses documented grok --help flags only: -p/--single with
// --always-approve and --output-format json. grok --help has no flag that
// disables AGENTS.md/CLAUDE.md discovery, so grokAgent does not implement
// GateInstructionNeutralizer. Isolation is --cwd + unique socket, not a
// verified neutralization knob. EnsureGateNeutralized therefore refuses grok
// when disable_project_settings is on.
type grokAgent struct {
	bin       string
	extraArgs []string
}

func (a *grokAgent) Name() string { return "grok" }

func (a *grokAgent) ReportsAgentAttempts() bool { return true }

func (a *grokAgent) Close() error { return nil }

func (a *grokAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "grok", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *grokAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	sockDir, err := os.MkdirTemp("", "nm-grok-leader-*")
	if err != nil {
		return nil, fmt.Errorf("grok leader socket dir: %w", err)
	}
	defer os.RemoveAll(sockDir)
	leaderSocket := filepath.Join(sockDir, "leader.sock")

	args := a.buildArgs(opts, leaderSocket)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil
	cmd.Env = grokSafeEnv(opts.CWD, opts.Env)
	shellenv.ConfigureShellCommand(cmd)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("grok start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "grok", pid)

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	stdout, readErr := io.ReadAll(started.stdout)
	waitErr := started.wait()
	stderrWG.Wait()
	if readErr != nil {
		retErr := fmt.Errorf("grok read stdout: %w", readErr)
		emitAgentExited(opts, "grok", pid, retErr)
		return nil, retErr
	}
	if waitErr != nil {
		detail := grokErrorDetail(stdout, string(stderrBuf))
		if detail != "" {
			retErr := fmt.Errorf("grok exited: %w: %s", waitErr, detail)
			emitAgentExited(opts, "grok", pid, retErr)
			return nil, retErr
		}
		retErr := fmt.Errorf("grok exited: %w", waitErr)
		emitAgentExited(opts, "grok", pid, retErr)
		return nil, retErr
	}

	text, usage, err := parseGrokJSONOutput(stdout)
	if err != nil {
		retErr := fmt.Errorf("grok output: %w", err)
		emitAgentExited(opts, "grok", pid, retErr)
		return nil, retErr
	}
	if opts.OnChunk != nil && text != "" {
		opts.OnChunk(text)
	}
	res, err := finalizeTextResult("grok", text, opts.JSONSchema, usage)
	emitAgentExited(opts, "grok", pid, err)
	return res, err
}

// buildArgs returns the grok argv for one invocation. User extras lead so
// operator flags such as --model take effect. Managed isolation and headless
// flags follow so --cwd, --leader-socket, --output-format, and -p cannot be
// displaced by extraArgs (those flags are also reserved in
// agent_args_override).
func (a *grokAgent) buildArgs(opts RunOpts, leaderSocket string) []string {
	args := make([]string, 0, len(a.extraArgs)+12)
	args = append(args, a.extraArgs...)
	args = append(args,
		"--cwd", opts.CWD,
		"--leader-socket", leaderSocket,
	)
	if !grokUserSetAlwaysApprove(a.extraArgs) {
		args = append(args, "--always-approve")
	}
	args = append(args, "--output-format", "json")
	if len(opts.JSONSchema) > 0 {
		args = append(args, "--json-schema", string(opts.JSONSchema))
	}
	args = append(args, "-p", opts.Prompt)
	return args
}

func grokUserSetAlwaysApprove(extraArgs []string) bool {
	for _, arg := range extraArgs {
		switch arg {
		case "--always-approve", "--yolo":
			return true
		}
	}
	return false
}

func grokSafeEnv(dir string, extra []string) []string {
	return stripEnvKeys(gitSafeEnv(dir, extra), grokIsolationEnvKeys...)
}

func stripEnvKeys(env []string, keys ...string) []string {
	drop := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if runtime.GOOS == "windows" {
			drop[strings.ToUpper(key)] = struct{}{}
		} else {
			drop[key] = struct{}{}
		}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		lookup := k
		if runtime.GOOS == "windows" {
			lookup = strings.ToUpper(k)
		}
		if _, skip := drop[lookup]; skip {
			continue
		}
		out = append(out, kv)
	}
	return out
}

type grokJSONResult struct {
	Type    string         `json:"type"`
	Message string         `json:"message"`
	Text    string         `json:"text"`
	Usage   *grokJSONUsage `json:"usage"`
}

type grokJSONUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	ReasoningTokens          int `json:"reasoning_tokens"`
}

func parseGrokJSONOutput(stdout []byte) (string, TokenUsage, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return "", TokenUsage{}, fmt.Errorf("empty stdout")
	}
	var result grokJSONResult
	if err := json.Unmarshal(trimmed, &result); err != nil {
		return "", TokenUsage{}, fmt.Errorf("parse json: %w (output snippet: %q)", err, outputSnippet(string(trimmed)))
	}
	if result.Type == "error" {
		msg := strings.TrimSpace(result.Message)
		if msg == "" {
			msg = "error"
		}
		return "", TokenUsage{}, fmt.Errorf("%s", msg)
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		return "", TokenUsage{}, fmt.Errorf("no text output")
	}
	var usage TokenUsage
	if result.Usage != nil {
		usage = TokenUsage{
			InputTokens:           result.Usage.InputTokens,
			OutputTokens:          result.Usage.OutputTokens,
			CacheReadTokens:       result.Usage.CacheReadInputTokens,
			CacheCreationTokens:   result.Usage.CacheCreationInputTokens,
			ReasoningTokens:       result.Usage.ReasoningTokens,
			Reported:              true,
			CacheCreationReported: true,
		}
	}
	return text, usage, nil
}

func grokErrorDetail(stdout []byte, stderr string) string {
	if _, _, err := parseGrokJSONOutput(stdout); err != nil {
		msg := strings.TrimSpace(err.Error())
		if msg != "" && msg != "empty stdout" && !strings.HasPrefix(msg, "parse json:") && msg != "no text output" {
			stderr = strings.TrimSpace(stderr)
			if stderr != "" {
				return msg + "; " + stderr
			}
			return msg
		}
	}
	return strings.TrimSpace(stderr)
}
