package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

const claudeScannerMaxTokenSize = 256 * 1024 * 1024

// claudeAgent spawns the claude CLI for each invocation.
type claudeAgent struct {
	bin string
}

func (a *claudeAgent) Name() string { return "claude" }

func (a *claudeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	args := a.buildArgs(opts.Prompt, opts.JSONSchema)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdout pipe: %w", err)
	}

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrR, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude start: %w", err)
	}

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(stderrR)
	}()

	var usage TokenUsage
	var result *claudeResult
	if err := parseClaudeEvents(ctx, stdout, opts.OnChunk, &usage, &result); err != nil {
		stderrWG.Wait()
		_ = cmd.Wait()
		return nil, fmt.Errorf("claude parse events: %w", err)
	}

	stderrWG.Wait()
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("claude exited: %w: %s", err, string(stderrBuf))
	}

	if result == nil {
		return nil, fmt.Errorf("claude returned no result event")
	}

	return finalizeClaudeResult(result, opts.JSONSchema, usage)
}

func (a *claudeAgent) Close() error { return nil }

func finalizeClaudeResult(result *claudeResult, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	if result.IsError || result.Subtype != "success" {
		return nil, fmt.Errorf("claude error: subtype=%s", result.Subtype)
	}
	if len(schema) > 0 && result.StructuredOutput == nil {
		return nil, fmt.Errorf("claude returned no structured output")
	}

	return &Result{
		Output: result.StructuredOutput,
		Text:   result.text,
		Usage:  usage,
	}, nil
}

// buildArgs constructs the claude CLI arguments.
func (a *claudeAgent) buildArgs(prompt string, schema json.RawMessage) []string {
	args := []string{
		"-p", prompt,
		"--verbose",
		"--output-format", "stream-json",
	}
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	}
	args = append(args, "--dangerously-skip-permissions")
	return args
}

// claudeEvent is the top-level JSONL event from claude CLI.
type claudeEvent struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`

	// result fields
	Subtype          string          `json:"subtype,omitempty"`
	IsError          bool            `json:"is_error,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	Usage            *claudeUsage    `json:"usage,omitempty"`
}

// claudeResult captures the parsed result event.
type claudeResult struct {
	Subtype          string
	IsError          bool
	StructuredOutput json.RawMessage
	text             string // accumulated text from assistant events
}

type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type claudeMessage struct {
	Usage   claudeUsage     `json:"usage"`
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// parseClaudeEvents reads JSONL from the reader and dispatches events.
// It accumulates token usage and captures the final result event.
func parseClaudeEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, result **claudeResult) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), claudeScannerMaxTokenSize)
	var textBuf string
	var hasEmittedText bool

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

		var event claudeEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue // skip malformed lines
		}

		switch event.Type {
		case "assistant":
			var msg claudeMessage
			if err := json.Unmarshal(event.Message, &msg); err != nil {
				continue
			}
			usage.Add(TokenUsage{
				InputTokens:         msg.Usage.InputTokens,
				OutputTokens:        msg.Usage.OutputTokens,
				CacheReadTokens:     msg.Usage.CacheReadInputTokens,
				CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
			})
			hasText := false
			for _, c := range msg.Content {
				if c.Type == "text" && c.Text != "" {
					hasText = true
					break
				}
			}
			if hasText && hasEmittedText && onChunk != nil {
				onChunk("\n\n")
			}
			for _, c := range msg.Content {
				if c.Type == "text" && c.Text != "" {
					textBuf += c.Text
					if onChunk != nil {
						onChunk(c.Text)
					}
					hasEmittedText = true
				}
			}

		case "result":
			if result != nil {
				*result = &claudeResult{
					Subtype:          event.Subtype,
					IsError:          event.IsError,
					StructuredOutput: event.StructuredOutput,
					text:             textBuf,
				}
			}
		}
	}

	return scanner.Err()
}
