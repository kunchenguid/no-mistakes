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
)

const acpxScannerMaxTokenSize = 256 * 1024 * 1024

type acpxAgent struct {
	bin        string
	target     string
	rawCommand string
}

func (a *acpxAgent) Name() string { return "acp:" + a.target }

func (a *acpxAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, a.Name(), opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *acpxAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	args := a.buildArgs(opts)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acpx stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("acpx stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acpx start: %w", err)
	}

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(stderr)
	}()

	var usage TokenUsage
	text, stdoutErr, err := parseAcpxJSONEvents(ctx, stdout, opts.OnChunk, &usage)
	stderrWG.Wait()
	if err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("acpx parse events: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("acpx exited: %w: %s", err, acpxProcessErrorOutput(stderrBuf, stdoutErr))
	}
	if usage.OutputTokens == 0 {
		usage.OutputTokens = estimateAcpxTokens(len(text))
	}
	return finalizeTextResult(a.Name(), text, opts.JSONSchema, usage)
}

func (a *acpxAgent) Close() error { return nil }

func (a *acpxAgent) buildArgs(opts RunOpts) []string {
	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt = buildACPStructuredPrompt(prompt, opts.JSONSchema)
	}

	args := make([]string, 0, 12)
	if a.rawCommand != "" {
		args = append(args, "--agent", a.rawCommand)
	}
	if opts.CWD != "" {
		args = append(args, "--cwd", opts.CWD)
	}
	args = append(args,
		"--format", "json",
		"--json-strict",
		"--approve-all",
		"--non-interactive-permissions", "deny",
		"--suppress-reads",
	)
	if a.rawCommand == "" {
		args = append(args, a.target)
	}
	args = append(args, "exec", prompt)
	return args
}

func acpxProcessErrorOutput(stderr []byte, stdoutErr string) string {
	parts := make([]string, 0, 2)
	if stderrText := strings.TrimSpace(string(stderr)); stderrText != "" {
		parts = append(parts, stderrText)
	}
	if stdoutErr != "" {
		parts = append(parts, stdoutErr)
	}
	return strings.Join(parts, "\n")
}

func buildACPStructuredPrompt(prompt string, schema json.RawMessage) string {
	return prompt + "\n\n## no-mistakes final output contract\n\n" +
		"When the task is complete, your final assistant message must be a single JSON object that matches this JSON Schema. " +
		"Return only the JSON object. Do not wrap it in Markdown fences. Do not include prose before or after the JSON.\n\n" +
		string(schema)
}

type acpxJSONMessage struct {
	Method string         `json:"method"`
	Error  *acpxJSONError `json:"error"`
	Params struct {
		Update acpxSessionUpdate `json:"update"`
	} `json:"params"`
}

type acpxJSONError struct {
	Message string `json:"message"`
}

type acpxSessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
	Text          string          `json:"text"`
	Used          int             `json:"used"`
}

func parseAcpxJSONEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage) (string, string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), acpxScannerMaxTokenSize)
	var output strings.Builder
	var stdoutErr string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return "", stdoutErr, ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg acpxJSONMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Error != nil && msg.Error.Message != "" && stdoutErr == "" {
			stdoutErr = msg.Error.Message
		}
		if msg.Method != "session/update" {
			continue
		}

		update := msg.Params.Update
		switch update.SessionUpdate {
		case "usage_update":
			if update.Used > usage.InputTokens {
				usage.InputTokens = update.Used
			}
		case "agent_message_chunk":
			text := acpxUpdateText(update)
			if text == "" {
				continue
			}
			output.WriteString(text)
			if onChunk != nil {
				onChunk(text)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", stdoutErr, err
	}
	return output.String(), stdoutErr, nil
}

func acpxUpdateText(update acpxSessionUpdate) string {
	if update.Text != "" {
		return update.Text
	}
	if len(update.Content) == 0 {
		return ""
	}
	var content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(update.Content, &content); err == nil && content.Text != "" {
		return content.Text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(update.Content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Text != "" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func estimateAcpxTokens(charCount int) int {
	if charCount <= 0 {
		return 0
	}
	return (charCount + 3) / 4
}
