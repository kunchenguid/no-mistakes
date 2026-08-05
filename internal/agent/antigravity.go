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

// antigravityAgent spawns the agy CLI for each invocation.
type antigravityAgent struct {
	bin       string
	extraArgs []string
}

func (a *antigravityAgent) Name() string { return "antigravity" }

func (a *antigravityAgent) ReportsAgentAttempts() bool { return true }

func (a *antigravityAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "antigravity", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *antigravityAgent) Close() error { return nil }

func (a *antigravityAgent) buildArgs(prompt, schemaPath string) []string {
	// Antigravity has strict flag parsing: only --print, --json-schema, --output-format
	// We append user extraArgs before the strict ones.
	args := make([]string, 0, len(a.extraArgs)+6)
	args = append(args, a.extraArgs...)
	args = append(args, "--print", prompt)
	if schemaPath != "" {
		args = append(args, "--json-schema", schemaPath)
	}
	args = append(args, "--output-format", "stream-json")
	return args
}

func (a *antigravityAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	schemaPath := ""
	if len(opts.JSONSchema) > 0 {
		f, err := os.CreateTemp("", "no-mistakes-antigravity-schema-*.json")
		if err != nil {
			return nil, fmt.Errorf("antigravity schema temp file: %w", err)
		}
		schemaPath = f.Name()
		if _, err := f.Write(opts.JSONSchema); err != nil {
			_ = f.Close()
			_ = os.Remove(schemaPath)
			return nil, fmt.Errorf("antigravity schema temp file write: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(schemaPath)
			return nil, fmt.Errorf("antigravity schema temp file close: %w", err)
		}
		defer os.Remove(schemaPath)
	}

	bin := a.bin
	if bin == "" {
		bin = "agy"
	}
	args := a.buildArgs(opts.Prompt, schemaPath)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = opts.CWD
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("antigravity start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "antigravity", pid)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	pp := &antigravityParser{onChunk: opts.OnChunk}
	if err := pp.parse(ctx, started.stdout); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		retErr := fmt.Errorf("antigravity parse events: %w", err)
		emitAgentExited(opts, "antigravity", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		stderr := strings.TrimSpace(string(stderrBuf))
		if stderr != "" {
			retErr := fmt.Errorf("antigravity exited: %w: %s", waitErr, stderr)
			emitAgentExited(opts, "antigravity", pid, retErr)
			return nil, retErr
		}
		retErr := fmt.Errorf("antigravity exited: %w", waitErr)
		emitAgentExited(opts, "antigravity", pid, retErr)
		return nil, retErr
	}

	if pp.errorMessage != "" {
		retErr := fmt.Errorf("antigravity reported error: %s", pp.errorMessage)
		emitAgentExited(opts, "antigravity", pid, retErr)
		return nil, retErr
	}

	text := pp.finalText()
	res, err := a.finalizeAntigravityResult(text, opts.JSONSchema, pp.usage)
	emitAgentExited(opts, "antigravity", pid, err)
	return res, err
}

func (a *antigravityAgent) finalizeAntigravityResult(text string, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	if text == "" {
		return nil, fmt.Errorf("antigravity returned no text output")
	}

	if len(schema) == 0 {
		return &Result{Text: text, Usage: usage, UsageReported: usage.Reported, CacheCreationReported: usage.CacheCreationReported}, nil
	}

	// 1. Try standard parsing
	var output json.RawMessage
	err := json.Unmarshal([]byte(text), &output)
	if err == nil {
		return &Result{Output: output, Text: text, Usage: usage, UsageReported: usage.Reported, CacheCreationReported: usage.CacheCreationReported}, nil
	}

	// 2. Bracket-matching backward search
	output, found := extractLastJSONObject(text)
	if found {
		return &Result{Output: output, Text: text, Usage: usage, UsageReported: usage.Reported, CacheCreationReported: usage.CacheCreationReported}, nil
	}

	// 3. Fallback wrapper to capture hallucinated text
	// Use a graceful failure object so the pipeline logs the mistake instead of crashing.
	escapedText, _ := json.Marshal("Agent hallucinated. Raw output: " + text)
	fallbackJSON := fmt.Sprintf(`{"success": false, "summary": %s, "error": %s, "raw_output": %s}`, escapedText, escapedText, escapedText)
	output = json.RawMessage(fallbackJSON)

	return &Result{Output: output, Text: text, Usage: usage, UsageReported: usage.Reported, CacheCreationReported: usage.CacheCreationReported}, nil
}

func extractLastJSONObject(text string) (json.RawMessage, bool) {
	openBraces := 0
	closeBraces := 0
	endIndex := -1
	runes := []rune(text)

	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == '}' {
			if endIndex == -1 {
				endIndex = i
			}
			closeBraces++
		} else if runes[i] == '{' {
			openBraces++
			if openBraces == closeBraces && endIndex != -1 {
				snippet := string(runes[i : endIndex+1])
				var parsed json.RawMessage
				if err := json.Unmarshal([]byte(snippet), &parsed); err == nil {
					return parsed, true
				}
				// Keep searching if this specific chunk wasn't actually valid JSON
			}
		}
	}
	return nil, false
}

type antigravityParser struct {
	onChunk func(string)

	streamText   string
	usage        TokenUsage
	errorMessage string
}

func (p *antigravityParser) parse(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)

	var sb strings.Builder

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

		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}

		if evtName, ok := event["event"].(string); ok {
			if evtName == "step_update" {
				if step, ok := event["step_update"].(map[string]any); ok {
					var delta string

					// Standard text and tool deltas
					if s, ok := step["text_delta"].(string); ok {
						delta += s
					}
					if s, ok := step["tool_call_delta"].(string); ok {
						delta += s
					}
					if s, ok := step["input_json_delta"].(string); ok {
						delta += s
					}
					if s, ok := step["arguments_delta"].(string); ok {
						delta += s
					}

					// Array-based tool calls
					if toolCalls, ok := step["tool_calls"].([]any); ok {
						for _, tcRaw := range toolCalls {
							if tc, ok := tcRaw.(map[string]any); ok {
								if s, ok := tc["delta"].(string); ok {
									delta += s
								}
								if s, ok := tc["input_json_delta"].(string); ok {
									delta += s
								}
								if s, ok := tc["arguments_delta"].(string); ok {
									delta += s
								}
								if fn, ok := tc["function"].(map[string]any); ok {
									if s, ok := fn["arguments"].(string); ok {
										delta += s
									}
								}
							}
						}
					}

					// Specialized payloads with newline padding
					if toolInfo, ok := step["tool_info"].(map[string]any); ok {
						if params, ok := toolInfo["parameters"]; ok {
							if paramStr, ok := params.(string); ok {
								delta += "\n" + paramStr + "\n"
							} else if paramBytes, err := json.Marshal(params); err == nil {
								delta += "\n" + string(paramBytes) + "\n"
							}
						}
					}

					if subagentInfo, ok := step["subagent_info"]; ok {
						if subagentStr, ok := subagentInfo.(string); ok {
							delta += "\n" + subagentStr + "\n"
						} else if subagentBytes, err := json.Marshal(subagentInfo); err == nil {
							delta += "\n" + string(subagentBytes) + "\n"
						}
					}

					if delta != "" {
						sb.WriteString(delta)
						if p.onChunk != nil {
							p.onChunk(delta)
						}
					}

					if usageMap, ok := step["usage"].(map[string]any); ok {
						p.usage.Reported = true
						if v, ok := usageMap["input_tokens"].(float64); ok {
							p.usage.InputTokens = int(v)
						}
						if v, ok := usageMap["output_tokens"].(float64); ok {
							p.usage.OutputTokens = int(v)
						}
						if v, ok := usageMap["cache_read_tokens"].(float64); ok {
							p.usage.CacheReadTokens = int(v)
						}
					}
				}
			} else if evtName == "result" {
				if result, ok := event["result"].(map[string]any); ok {
					if status, _ := result["status"].(string); status == "ERROR" {
						if resp, _ := result["response"].(string); resp != "" {
							p.errorMessage = resp
						} else {
							p.errorMessage = "unknown error"
						}
					}

					if usageMap, ok := result["usage"].(map[string]any); ok {
						p.usage.Reported = true
						if v, ok := usageMap["input_tokens"].(float64); ok {
							p.usage.InputTokens = int(v)
						}
						if v, ok := usageMap["output_tokens"].(float64); ok {
							p.usage.OutputTokens = int(v)
						}
						if v, ok := usageMap["cache_read_tokens"].(float64); ok {
							p.usage.CacheReadTokens = int(v)
						}
						if v, ok := usageMap["cache_creation_tokens"].(float64); ok {
							p.usage.CacheCreationTokens = int(v)
							p.usage.CacheCreationReported = true
						}
					}

					if output, ok := result["structured_output"]; ok && output != nil {
						if outBytes, err := json.Marshal(output); err == nil {
							// If we received structured_output, it supersedes the stream.
							sb.Reset()
							sb.Write(outBytes)
						}
					}
				}
			}
		}
	}

	p.streamText = sb.String()
	return scanner.Err()
}

func (p *antigravityParser) finalText() string {
	return strings.TrimSpace(p.streamText)
}
