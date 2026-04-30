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

// piAgent spawns the pi CLI for each invocation. Pi reads its prompt from
// stdin and emits JSONL on stdout when --mode json is set. The lifecycle is
// codex-shaped: one process per Run, no managed server.
type piAgent struct {
	bin       string
	extraArgs []string
}

func (a *piAgent) Name() string { return "pi" }

func (a *piAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "pi", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *piAgent) Close() error { return nil }

func (a *piAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	args := a.buildArgs()
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pi stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pi stdout pipe: %w", err)
	}
	stderrR, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("pi stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pi start: %w", err)
	}

	prompt := buildPiPrompt(opts.Prompt, opts.JSONSchema)
	go func() {
		defer stdin.Close()
		_, _ = io.WriteString(stdin, prompt)
	}()

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(stderrR)
	}()

	pp := &piParser{onChunk: opts.OnChunk}
	if err := pp.parse(ctx, stdout); err != nil {
		stderrWG.Wait()
		_ = cmd.Wait()
		return nil, fmt.Errorf("pi parse events: %w", err)
	}

	stderrWG.Wait()
	if err := cmd.Wait(); err != nil {
		stderr := strings.TrimSpace(string(stderrBuf))
		if stderr != "" {
			return nil, fmt.Errorf("pi exited: %w: %s", err, stderr)
		}
		return nil, fmt.Errorf("pi exited: %w", err)
	}

	if pp.assistantError != "" {
		return nil, fmt.Errorf("pi reported error: %s", pp.assistantError)
	}

	text := pp.finalText()
	return finalizeTextResult("pi", text, opts.JSONSchema, pp.usage)
}

// buildArgs returns the Pi argv for one invocation. User extras come first
// (so user --provider/--model take effect), then the managed flags that
// no-mistakes requires for JSONL parsing.
func (a *piAgent) buildArgs() []string {
	args := make([]string, 0, len(a.extraArgs)+3)
	args = append(args, a.extraArgs...)
	args = append(args, "--mode", "json", "--no-session")
	return args
}

// buildPiPrompt appends a JSON-output contract to the user prompt when a
// schema is provided. Pi has no equivalent of codex's --output-schema flag,
// so we inline the schema in the prompt the same way gnhf does.
func buildPiPrompt(prompt string, schema json.RawMessage) string {
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

// piParser tracks the streaming state of one Pi run. It accumulates text
// deltas, captures the final assistant text and usage, and surfaces any
// reported assistant error.
type piParser struct {
	onChunk func(string)

	streamText     map[int]string
	completeText   map[int]string
	finalAssistant map[string]any
	usage          TokenUsage
	assistantError string
}

func (p *piParser) parse(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)

	p.streamText = make(map[int]string)
	p.completeText = make(map[int]string)

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
		p.handleEvent(event)
	}

	return scanner.Err()
}

func (p *piParser) handleEvent(event map[string]any) {
	typ, _ := event["type"].(string)
	switch typ {
	case "message_update":
		p.rememberAssistant(event["message"])
		p.handleAssistantEvent(event["assistantMessageEvent"])
	case "message_end", "turn_end":
		p.rememberAssistant(event["message"])
	case "agent_end":
		// Fallback: pull the final assistant message from the conversation
		// when no streaming message_end carried it.
		if p.finalAssistant != nil {
			return
		}
		messages, ok := event["messages"].([]any)
		if !ok {
			return
		}
		for i := len(messages) - 1; i >= 0; i-- {
			if msg, ok := messages[i].(map[string]any); ok && msg["role"] == "assistant" {
				p.rememberAssistant(msg)
				return
			}
		}
	}
}

func (p *piParser) rememberAssistant(raw any) {
	msg, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if role, _ := msg["role"].(string); role != "assistant" {
		return
	}
	p.finalAssistant = msg

	if reason, _ := msg["stopReason"].(string); reason == "error" || reason == "aborted" {
		p.assistantError = piFirstString(msg, "errorMessage", "error", "message")
		if p.assistantError == "" {
			p.assistantError = fmt.Sprintf("stopReason=%s", reason)
		}
	}

	if usage, ok := msg["usage"].(map[string]any); ok {
		p.usage = piUsageFrom(usage)
	}
}

func (p *piParser) handleAssistantEvent(raw any) {
	evt, ok := raw.(map[string]any)
	if !ok {
		return
	}
	idx := piIntField(evt, "contentIndex", "content_index")
	switch evt["type"] {
	case "text_delta":
		// Emit just the incremental delta. no-mistakes' OnChunk consumers
		// (TUI log line buffer, file logger) expect appended text, not
		// cumulative state.
		delta := piFirstString(evt, "delta", "text", "content")
		if delta == "" {
			return
		}
		p.streamText[idx] += delta
		if p.onChunk != nil {
			p.onChunk(delta)
		}
	case "text_end":
		// Capture the complete text for final-result resolution. Don't
		// re-emit to OnChunk: the deltas already covered it. If the event
		// carries the full text (Pi's normal shape), prefer that over the
		// delta accumulator since it's authoritative.
		text := piFirstString(evt, "text", "content")
		if text == "" {
			text = p.streamText[idx]
		}
		p.completeText[idx] = text
	}
}

// finalText returns the final assistant text, preferring (in order) the
// content of the last assistant message, the text_end-completed deltas, and
// finally the in-flight stream buffer.
func (p *piParser) finalText() string {
	if text := strings.TrimSpace(textFromAssistantMessage(p.finalAssistant)); text != "" {
		return text
	}
	if text := strings.TrimSpace(joinByIndex(p.completeText)); text != "" {
		return text
	}
	return strings.TrimSpace(joinByIndex(p.streamText))
}

func textFromAssistantMessage(msg map[string]any) string {
	if msg == nil {
		return ""
	}
	switch v := msg["content"].(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, block := range v {
			switch t := block.(type) {
			case string:
				b.WriteString(t)
			case map[string]any:
				if s, ok := t["text"].(string); ok {
					b.WriteString(s)
					continue
				}
				if s, ok := t["content"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	if s, ok := msg["text"].(string); ok {
		return s
	}
	return ""
}

func joinByIndex(parts map[int]string) string {
	if len(parts) == 0 {
		return ""
	}
	max := -1
	for k := range parts {
		if k > max {
			max = k
		}
	}
	var b strings.Builder
	for i := 0; i <= max; i++ {
		b.WriteString(parts[i])
	}
	return b.String()
}

func piFirstString(m map[string]any, names ...string) string {
	for _, n := range names {
		if v, ok := m[n].(string); ok {
			return v
		}
	}
	return ""
}

func piIntField(m map[string]any, names ...string) int {
	for _, n := range names {
		switch v := m[n].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			if i, err := v.Int64(); err == nil {
				return int(i)
			}
		}
	}
	return 0
}

func piUsageFrom(usage map[string]any) TokenUsage {
	return TokenUsage{
		InputTokens:         piIntField(usage, "input"),
		OutputTokens:        piIntField(usage, "output"),
		CacheReadTokens:     piIntField(usage, "cacheRead"),
		CacheCreationTokens: piIntField(usage, "cacheWrite"),
	}
}
