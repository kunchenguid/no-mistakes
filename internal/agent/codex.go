package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

// codexAgent spawns the codex CLI for each invocation.
type codexAgent struct {
	bin       string
	extraArgs []string
}

func (a *codexAgent) Name() string { return "codex" }

func (a *codexAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	schemaPath := ""
	if len(opts.JSONSchema) > 0 {
		f, err := os.CreateTemp("", "no-mistakes-codex-schema-*.json")
		if err != nil {
			return nil, fmt.Errorf("codex schema temp file: %w", err)
		}
		schemaPath = f.Name()
		schema, err := codexOutputSchema(opts.JSONSchema)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(schemaPath)
			return nil, fmt.Errorf("codex schema normalize: %w", err)
		}
		if _, err := f.Write(schema); err != nil {
			_ = f.Close()
			_ = os.Remove(schemaPath)
			return nil, fmt.Errorf("codex schema temp file write: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(schemaPath)
			return nil, fmt.Errorf("codex schema temp file close: %w", err)
		}
		defer os.Remove(schemaPath)
	}

	args := a.buildArgs(opts.Prompt, schemaPath)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Stdin = nil

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex stdout pipe: %w", err)
	}

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrR, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("codex stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex start: %w", err)
	}

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(stderrR)
	}()

	var usage TokenUsage
	var lastMessage string
	var codexErr string
	if err := parseCodexEvents(ctx, stdout, opts.OnChunk, &usage, &lastMessage, &codexErr); err != nil {
		stderrWG.Wait()
		_ = cmd.Wait()
		return nil, fmt.Errorf("codex parse events: %w", err)
	}

	stderrWG.Wait()
	if err := cmd.Wait(); err != nil {
		detail := strings.TrimSpace(codexErr)
		stderr := strings.TrimSpace(string(stderrBuf))
		if detail != "" && stderr != "" {
			detail += "; " + stderr
		} else if detail == "" {
			detail = stderr
		}
		return nil, fmt.Errorf("codex exited: %w: %s", err, detail)
	}

	return finalizeTextResult("codex", lastMessage, opts.JSONSchema, usage)
}

func (a *codexAgent) Close() error { return nil }

// buildArgs constructs the codex CLI arguments. User-supplied extraArgs are
// inserted between "exec" and the prompt so user flags (e.g. -m, --sandbox)
// take effect. If the user declared their own execution-mode flag, the
// default --dangerously-bypass-approvals-and-sandbox is not added.
func (a *codexAgent) buildArgs(prompt, schemaPath string) []string {
	args := make([]string, 0, len(a.extraArgs)+8)
	args = append(args, "exec")
	args = append(args, a.extraArgs...)
	args = append(args, prompt, "--json")
	if schemaPath != "" {
		args = append(args, "--output-schema", schemaPath)
	}
	if !codexUserSetExecutionMode(a.extraArgs) {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	args = append(args, "--color", "never")
	return args
}

// codexUserSetExecutionMode reports whether extraArgs already declare an
// execution/sandbox flag that conflicts with the default bypass.
func codexUserSetExecutionMode(extraArgs []string) bool {
	for _, arg := range extraArgs {
		switch {
		case arg == "--dangerously-bypass-approvals-and-sandbox",
			arg == "--ask-for-approval",
			arg == "--sandbox":
			return true
		case strings.HasPrefix(arg, "--ask-for-approval="),
			strings.HasPrefix(arg, "--sandbox="):
			return true
		}
	}
	return false
}

// codexEvent is the top-level JSONL event from codex CLI.
type codexEvent struct {
	Type    string      `json:"type"`
	Item    *codexItem  `json:"item,omitempty"`
	Usage   *codexUsage `json:"usage,omitempty"`
	Message string      `json:"message,omitempty"`
}

type codexItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

// parseCodexEvents reads JSONL from the reader and dispatches events.
// It captures the last agent_message text and accumulates token usage.
func parseCodexEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, lastMessage *string, codexErr *string) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)

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

		var event codexEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue // skip malformed lines
		}

		switch event.Type {
		case "error":
			if event.Message != "" && codexErr != nil {
				*codexErr = event.Message
			}

		case "item.completed":
			if event.Item != nil && event.Item.Type == "agent_message" {
				*lastMessage = event.Item.Text
				if onChunk != nil {
					onChunk(event.Item.Text)
				}
			}

		case "turn.completed":
			if event.Usage != nil {
				usage.Add(TokenUsage{
					InputTokens:     event.Usage.InputTokens,
					OutputTokens:    event.Usage.OutputTokens,
					CacheReadTokens: event.Usage.CachedInputTokens,
				})
			}
		}
	}

	return scanner.Err()
}

func codexOutputSchema(schema json.RawMessage) ([]byte, error) {
	var value any
	if err := json.Unmarshal(schema, &value); err != nil {
		return nil, err
	}
	addAdditionalPropertiesFalse(value)
	return json.Marshal(value)
}

func addAdditionalPropertiesFalse(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	required := requiredSet(schema)
	if schema["type"] == "object" {
		if _, ok := schema["additionalProperties"]; !ok {
			schema["additionalProperties"] = false
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		if schema["type"] == "object" {
			schema["required"] = names
		}
		for _, name := range names {
			property := properties[name]
			addAdditionalPropertiesFalse(property)
			if !required[name] {
				allowSchemaNull(property)
			}
		}
	}
	if items, ok := schema["items"]; ok {
		addAdditionalPropertiesFalse(items)
	}
}

func requiredSet(schema map[string]any) map[string]bool {
	required := make(map[string]bool)
	items, _ := schema["required"].([]any)
	for _, item := range items {
		name, ok := item.(string)
		if ok {
			required[name] = true
		}
	}
	return required
}

func allowSchemaNull(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	if enum, ok := schema["enum"].([]any); ok && !containsNil(enum) {
		schema["enum"] = append(enum, nil)
	}
	switch typ := schema["type"].(type) {
	case string:
		if typ != "null" {
			schema["type"] = []any{typ, "null"}
		}
	case []any:
		if !containsString(typ, "null") {
			schema["type"] = append(typ, "null")
		}
	}
}

func containsNil(items []any) bool {
	for _, item := range items {
		if item == nil {
			return true
		}
	}
	return false
}

func containsString(items []any, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
