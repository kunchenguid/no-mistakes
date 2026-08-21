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

// SupportsSessionResume reports antigravity's durable-session capability:
// stream-json events carry the conversation identity, and
// `--conversation <id>` reopens that conversation headless.
func (a *antigravityAgent) SupportsSessionResume() bool { return true }

// SupportsSessionProvider accepts sessions minted under either spelling of
// the provider name, so a session recorded before a config rename still
// resumes.
func (a *antigravityAgent) SupportsSessionProvider(provider string) bool {
	return provider == "antigravity" || provider == "agy"
}

func (a *antigravityAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "antigravity", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *antigravityAgent) Close() error { return nil }

func (a *antigravityAgent) buildArgs(prompt, schemaPath, sessionID string) []string {
	// Antigravity has strict flag parsing: only --print, --json-schema, --output-format
	// We append user extraArgs before the strict ones.
	args := make([]string, 0, len(a.extraArgs)+9)
	args = append(args, a.extraArgs...)
	if sessionID != "" {
		args = append(args, "--conversation", sessionID)
	}
	args = append(args, "--dangerously-skip-permissions")
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
	requestedSession := ""
	if opts.Session != nil {
		requestedSession = opts.Session.ID
	}
	args := a.buildArgs(opts.Prompt, schemaPath, requestedSession)
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
	res, err := finalizeTextResult("antigravity", text, opts.JSONSchema, pp.usage)
	if res != nil && pp.sessionID != "" {
		res.SessionID = pp.sessionID
		res.Provider = "antigravity"
		res.Resumed = requestedSession != "" && requestedSession == pp.sessionID
	}
	emitAgentExited(opts, "antigravity", pid, err)
	return res, err
}

type antigravityParser struct {
	onChunk func(string)

	streamText   string
	sessionID    string
	structured   string
	response     string
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

		// init carries the conversation identity at the top level; every
		// event of the run names the conversation actually serving it.
		if id, ok := event["conversation_id"].(string); ok && id != "" {
			p.sessionID = id
		}

		if evtName, ok := event["event"].(string); ok {
			if evtName == "step_update" {
				if step, ok := event["step_update"].(map[string]any); ok {
					var delta string

					if id, ok := step["conversation_id"].(string); ok && id != "" {
						p.sessionID = id
					}

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
						applyAgyReasoningUsage(&p.usage, usageMap)
					}
				}
			} else if evtName == "result" {
				if result, ok := event["result"].(map[string]any); ok {
					if id, ok := result["conversation_id"].(string); ok && id != "" {
						p.sessionID = id
					}
					if status, _ := result["status"].(string); status == "ERROR" {
						if resp, _ := result["error"].(string); resp != "" {
							p.errorMessage = resp
						} else {
							p.errorMessage = "unknown error"
						}
					}
					// The terminal answer is authoritative wherever agy puts it:
					// result.response outranks stream deltas even when some were
					// collected, and structured_output outranks both (finalText).
					if resp, _ := result["response"].(string); resp != "" {
						p.response = resp
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
						applyAgyReasoningUsage(&p.usage, usageMap)
					}

					if output, ok := result["structured_output"]; ok && output != nil {
						if outBytes, err := json.Marshal(output); err == nil {
							p.structured = string(outBytes)
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
	switch {
	case p.structured != "":
		return strings.TrimSpace(p.structured)
	case p.response != "":
		return strings.TrimSpace(p.response)
	default:
		return strings.TrimSpace(p.streamText)
	}
}

// applyAgyReasoningUsage records agy's thinking_tokens as reasoning output so
// reasoning-model invocations are not undercounted. Presence, not the value,
// sets ReasoningReported so a genuine zero stays distinguishable from an
// adapter that never exposes the field.
func applyAgyReasoningUsage(usage *TokenUsage, usageMap map[string]any) {
	if _, ok := usageMap["thinking_tokens"]; !ok {
		return
	}
	usage.ReasoningReported = true
	if v, ok := usageMap["thinking_tokens"].(float64); ok {
		usage.ReasoningTokens = int(v)
	}
}
