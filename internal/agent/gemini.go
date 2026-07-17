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

type geminiAgent struct {
	bin       string
	extraArgs []string
}

func (a *geminiAgent) Name() string { return "gemini" }

const geminiMaxRetries = 3
const geminiScannerMaxTokenSize = 256 * 1024 * 1024

func geminiRetryClassifier(err error) (string, bool) {
	if strings.Contains(err.Error(), "429") || strings.Contains(strings.ToLower(err.Error()), "quota exceeded") {
		return "quota exceeded", true
	}
	if strings.Contains(err.Error(), "JSON output missing required field") || strings.Contains(err.Error(), "schema validation") {
		return "schema validation failed", true
	}
	return classifyTransient(err)
}

func (a *geminiAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "gemini", opts, geminiMaxRetries, geminiRetryClassifier, nil, func() (*Result, error) {
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
	var result *geminiResult
	if err := parseGeminiEvents(ctx, started.stdout, opts.OnChunk, &usage, &result); err != nil {
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

	res, err := finalizeGeminiResult(result, opts.JSONSchema, usage)
	emitAgentExited(opts, "gemini", pid, err)
	return res, err
}

func (a *geminiAgent) Close() error { return nil }

func (a *geminiAgent) buildArgs(prompt string, schema json.RawMessage) []string {
	if len(schema) > 0 {
		prompt = prompt + "\n\nCRITICAL: You must output your final answer as a single structured JSON block. Wrap your JSON in standard markdown fences (```json ... ```) so it can be extracted. It must strictly match this schema:\n```json\n" + string(schema) + "\n```"
	}
	args := make([]string, 0, len(a.extraArgs)+10)
	args = append(args, a.extraArgs...)
	args = append(args,
		"-p", prompt,
		"--output-format", "stream-json",
	)
	if !geminiUserSetModel(a.extraArgs) {
		args = append(args, "--model", "gemini-3.1-pro-preview-customtools")
	}
	if !geminiUserSetPermissionMode(a.extraArgs) {
		args = append(args, "-y", "--no-sandbox")
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
		if arg == "-y" || arg == "--yolo" || arg == "--no-sandbox" {
			return true
		}
	}
	return false
}

type geminiEvent struct {
	Type    string `json:"type"`
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	Status  string `json:"status,omitempty"`
	Stats   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"stats,omitempty"`
}

type geminiResult struct {
	Status string
	Text   string
}

func parseGeminiEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, result **geminiResult) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), geminiScannerMaxTokenSize)
	var textBuf string

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

		var event geminiEvent
		if err := json.Unmarshal(line, &event); err != nil {
			textBuf += string(line) + "\n"
			if onChunk != nil {
				onChunk(string(line) + "\n")
			}
			continue
		}

		switch event.Type {
		case "message":
			if event.Role == "assistant" && event.Content != "" {
				textBuf += event.Content
				if onChunk != nil {
					onChunk(event.Content)
				}
			}
		case "result":
			if result != nil {
				*result = &geminiResult{
					Status: event.Status,
					Text:   textBuf,
				}
				usage.InputTokens = event.Stats.InputTokens
				usage.OutputTokens = event.Stats.OutputTokens
			}
		case "init", "tool_use", "tool_result", "tool_call":
			// Ignore these system events so they don't pollute textBuf
		default:
			textBuf += string(line) + "\n"
			if onChunk != nil {
				onChunk(string(line) + "\n")
			}
		}
	}
	err := scanner.Err()
	if err == nil && result != nil && *result == nil && textBuf != "" {
		*result = &geminiResult{
			Status: "success",
			Text:   textBuf,
		}
	}
	return err
}

func finalizeGeminiResult(result *geminiResult, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	if result.Status != "success" {
		return nil, fmt.Errorf("gemini error: status=%s", result.Status)
	}
	return finalizeTextResult("gemini", result.Text, schema, usage)
}
