package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// antigravityAgent spawns the agy CLI for each invocation.
// agy runs non-interactively with `agy -p <prompt> --output-format stream-json --dangerously-skip-permissions`.
type antigravityAgent struct {
	bin                    string
	extraArgs              []string
	disableProjectSettings bool
}

func (a *antigravityAgent) Name() string { return "antigravity" }

func (a *antigravityAgent) ReportsAgentAttempts() bool { return true }

func (a *antigravityAgent) SupportsSessionResume() bool { return true }

func (a *antigravityAgent) SupportsSessionProvider(provider string) bool {
	return provider == "antigravity" || provider == "agy"
}

// NeutralizesGateInstructions reports whether antigravity is currently launched with
// the target repo's project agent-instruction files suppressed.
func (a *antigravityAgent) NeutralizesGateInstructions() bool {
	return a.disableProjectSettings
}

func (a *antigravityAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "antigravity", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *antigravityAgent) Close() error { return nil }

func (a *antigravityAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	prompt := buildAntigravityPrompt(opts.Prompt, opts.JSONSchema)
	args := a.buildArgs(prompt, opts)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil
	cmd.Env = gitSafeEnv(opts.CWD, opts.Env)
	shellenv.ConfigureShellCommand(cmd)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("antigravity start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "antigravity", pid)

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	var (
		usage            TokenUsage
		sessionID        string
		status           = "SUCCESS"
		resultResponse   string
		resultStructured json.RawMessage
		resultError      string
	)

	if err := parseAntigravityEvents(ctx, started.stdout, opts.OnChunk, &usage, &sessionID, &status, &resultResponse, &resultStructured, &resultError); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		retErr := fmt.Errorf("antigravity parse events: %w", err)
		emitAgentExited(opts, "antigravity", pid, retErr)
		return nil, retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()

	stderr := strings.TrimSpace(string(stderrBuf))
	if waitErr != nil {
		detail := antigravityErrorDetail(resultError, stderr)
		if detail != "" {
			retErr := fmt.Errorf("antigravity exited: %w: %s", waitErr, detail)
			emitAgentExited(opts, "antigravity", pid, retErr)
			return nil, retErr
		}
		retErr := fmt.Errorf("antigravity exited: %w", waitErr)
		emitAgentExited(opts, "antigravity", pid, retErr)
		return nil, retErr
	}

	text := strings.TrimSpace(resultResponse)
	if len(text) == 0 && len(resultStructured) > 0 {
		text = string(resultStructured)
	}

	if !usage.Reported {
		if len(text) > 0 {
			usage.OutputTokens = (len(text) + 3) / 4
		}
		if len(prompt) > 0 {
			usage.InputTokens = (len(prompt) + 3) / 4
		}
	}

	res, err := finalizeTextResult("antigravity", text, opts.JSONSchema, usage)
	if err != nil && len(resultStructured) > 0 {
		if structRes, structErr := finalizeTextResult("antigravity", string(resultStructured), opts.JSONSchema, usage); structErr == nil {
			res = structRes
			err = nil
		}
	}
	if err != nil && len(opts.JSONSchema) > 0 && text != "" {
		if candidate, found := extractLastJSONObject(text); found {
			if structRes, structErr := finalizeTextResult("antigravity", string(candidate), opts.JSONSchema, usage); structErr == nil {
				res = structRes
				err = nil
			}
		}
	}

	if (status == "ERROR" || resultError != "") && res == nil {
		detail := antigravityErrorDetail(resultError, stderr)
		if detail == "" {
			detail = "agent returned error status"
		}
		retErr := fmt.Errorf("antigravity error: %s", detail)
		emitAgentExited(opts, "antigravity", pid, retErr)
		return nil, retErr
	}

	if res != nil {
		if sessionID != "" {
			res.SessionID = sessionID
		} else if opts.Session != nil && opts.Session.ID != "" {
			res.SessionID = opts.Session.ID
		}
		if opts.Session != nil && opts.Session.ID != "" {
			res.Resumed = (opts.Session.ID == res.SessionID)
		}
		res.Provider = "antigravity"
	}

	emitAgentExited(opts, "antigravity", pid, err)
	return res, err
}

func (a *antigravityAgent) buildArgs(prompt string, opts RunOpts) []string {
	args := make([]string, 0, len(a.extraArgs)+12)
	if a.disableProjectSettings {
		args = append(args, "--disable-slash-commands")
	}
	args = append(args, a.extraArgs...)
	args = append(args,
		"-p", prompt,
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
		"--print-timeout", "30m",
	)
	if opts.Session == nil || opts.Session.ID == "" {
		args = append(args, "--new-project")
	}
	if len(opts.JSONSchema) > 0 {
		args = append(args, "--json-schema", string(opts.JSONSchema))
	}
	if opts.Session != nil && opts.Session.ID != "" {
		args = append(args, "--conversation", opts.Session.ID)
	}
	return args
}

func buildAntigravityPrompt(prompt string, schema json.RawMessage) string {
	if len(schema) == 0 {
		return prompt
	}
	pretty, err := json.MarshalIndent(json.RawMessage(schema), "", "  ")
	if err != nil {
		pretty = []byte(schema)
	}
	return prompt + "\n\n## no-mistakes final output contract\n\n" +
		"When the task is complete, your final assistant response must be only valid JSON matching this JSON Schema. " +
		"Do not wrap it in Markdown fences. Do not include prose before or after the JSON object.\n\n" +
		string(pretty)
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
			}
		}
	}
	return nil, false
}

type agyStreamEvent struct {
	Event          string         `json:"event"`
	ConversationID string         `json:"conversation_id"`
	Init           *agyInitData   `json:"init,omitempty"`
	StepUpdate     *agyStepUpdate `json:"step_update,omitempty"`
	Result         *agyResultData `json:"result,omitempty"`
}

type agyInitData struct {
	CWD   string   `json:"cwd"`
	Tools []string `json:"tools"`
}

type agyStepUpdate struct {
	ConversationID  string          `json:"conversation_id"`
	StepIndex       int             `json:"step_index"`
	State           string          `json:"state"`
	StepType        string          `json:"step_type"`
	TextDelta       string          `json:"text_delta"`
	ToolCallDelta   string          `json:"tool_call_delta"`
	InputJSONDelta  string          `json:"input_json_delta"`
	ArgumentsDelta  string          `json:"arguments_delta"`
	ToolCalls       []agyToolCall   `json:"tool_calls,omitempty"`
	ToolInfo        json.RawMessage `json:"tool_info,omitempty"`
	SubagentInfo    json.RawMessage `json:"subagent_info,omitempty"`
	DurationSeconds float64         `json:"duration_seconds"`
	Usage           *agyUsageData   `json:"usage,omitempty"`
}

type agyToolCall struct {
	Delta          string          `json:"delta,omitempty"`
	InputJSONDelta string          `json:"input_json_delta,omitempty"`
	ArgumentsDelta string          `json:"arguments_delta,omitempty"`
	Function       *agyFunctionArg `json:"function,omitempty"`
}

type agyFunctionArg struct {
	Arguments string `json:"arguments,omitempty"`
}

type agyResultData struct {
	ConversationID   string          `json:"conversation_id"`
	Status           string          `json:"status"`
	Response         string          `json:"response"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	Error            string          `json:"error,omitempty"`
	DurationSeconds  float64         `json:"duration_seconds"`
	NumTurns         int             `json:"num_turns"`
	Usage            *agyUsageData   `json:"usage,omitempty"`
}

type agyUsageData struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	ThinkingTokens      int `json:"thinking_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
	TotalTokens         int `json:"total_tokens"`
}

func parseAntigravityEvents(
	ctx context.Context,
	r io.Reader,
	onChunk func(string),
	usage *TokenUsage,
	sessionID *string,
	status *string,
	resultResponse *string,
	resultStructured *json.RawMessage,
	resultError *string,
) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)

	var textDeltaAccum strings.Builder

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}

		var event agyStreamEvent
		if err := json.Unmarshal(trimmed, &event); err != nil {
			if onChunk != nil {
				onChunk(string(line) + "\n")
			}
			continue
		}

		if event.ConversationID != "" && *sessionID == "" {
			*sessionID = event.ConversationID
		}

		switch event.Event {
		case "init":
			if event.ConversationID != "" {
				*sessionID = event.ConversationID
			}
		case "step_update":
			if event.StepUpdate != nil {
				if event.StepUpdate.ConversationID != "" && *sessionID == "" {
					*sessionID = event.StepUpdate.ConversationID
				}

				var delta strings.Builder
				if event.StepUpdate.TextDelta != "" {
					delta.WriteString(event.StepUpdate.TextDelta)
				}
				if event.StepUpdate.ToolCallDelta != "" {
					delta.WriteString(event.StepUpdate.ToolCallDelta)
				}
				if event.StepUpdate.InputJSONDelta != "" {
					delta.WriteString(event.StepUpdate.InputJSONDelta)
				}
				if event.StepUpdate.ArgumentsDelta != "" {
					delta.WriteString(event.StepUpdate.ArgumentsDelta)
				}
				for _, tc := range event.StepUpdate.ToolCalls {
					if tc.Delta != "" {
						delta.WriteString(tc.Delta)
					}
					if tc.InputJSONDelta != "" {
						delta.WriteString(tc.InputJSONDelta)
					}
					if tc.ArgumentsDelta != "" {
						delta.WriteString(tc.ArgumentsDelta)
					}
					if tc.Function != nil && tc.Function.Arguments != "" {
						delta.WriteString(tc.Function.Arguments)
					}
				}
				if len(event.StepUpdate.ToolInfo) > 0 {
					delta.WriteString("\n" + string(event.StepUpdate.ToolInfo) + "\n")
				}
				if len(event.StepUpdate.SubagentInfo) > 0 {
					delta.WriteString("\n" + string(event.StepUpdate.SubagentInfo) + "\n")
				}

				deltaStr := delta.String()
				if deltaStr != "" {
					textDeltaAccum.WriteString(deltaStr)
					if onChunk != nil {
						onChunk(deltaStr)
					}
				}

				if event.StepUpdate.Usage != nil {
					applyAgyUsage(usage, event.StepUpdate.Usage)
				}
			}
		case "result":
			if event.Result != nil {
				if event.Result.ConversationID != "" {
					*sessionID = event.Result.ConversationID
				}
				if event.Result.Status != "" {
					*status = event.Result.Status
				}
				if event.Result.Response != "" {
					*resultResponse = event.Result.Response
				}
				if len(event.Result.StructuredOutput) > 0 {
					*resultStructured = event.Result.StructuredOutput
				}
				if event.Result.Error != "" {
					*resultError = event.Result.Error
				}
				if event.Result.Usage != nil {
					applyAgyUsage(usage, event.Result.Usage)
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	if *resultResponse == "" && textDeltaAccum.Len() > 0 {
		*resultResponse = textDeltaAccum.String()
	}

	return nil
}

func applyAgyUsage(target *TokenUsage, src *agyUsageData) {
	if src == nil {
		return
	}
	target.InputTokens = src.InputTokens
	target.OutputTokens = src.OutputTokens
	target.ReasoningTokens = src.ThinkingTokens
	target.CacheReadTokens = src.CacheReadTokens
	if src.CacheCreationTokens > 0 {
		target.CacheCreationTokens = src.CacheCreationTokens
		target.CacheCreationReported = true
	}
	target.Reported = true
}

func antigravityErrorDetail(resultErr, stderr string) string {
	parts := make([]string, 0, 2)
	if resultErr = strings.TrimSpace(resultErr); resultErr != "" {
		parts = append(parts, resultErr)
	}
	if stderr = strings.TrimSpace(stderr); stderr != "" {
		parts = append(parts, stderr)
	}
	return strings.Join(parts, "; ")
}
