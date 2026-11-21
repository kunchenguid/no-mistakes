package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ompEventParser tracks the streaming state of one omp JSON-mode run. omp
// emits a pi-lineage JSONL event vocabulary: session, message_start,
// message_update, message_end, turn_end, agent_end, and tool_execution_end.
// It accumulates text deltas, captures the final assistant text and usage, and
// surfaces any reported assistant error.
type ompEventParser struct {
	onChunk func(string)

	streamText     map[int]string
	completeText   map[int]string
	finalAssistant map[string]any
	model          string
	provider       string
	usage          TokenUsage
	seenUsage      map[string]struct{}
	assistantError string
	// agentEnded is set once omp's agent_end event arrives: the run finished
	// and its result is complete even if the process exit is not observed.
	agentEnded bool
}

func (p *ompEventParser) parse(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)

	p.streamText = make(map[int]string)
	p.completeText = make(map[int]string)
	p.seenUsage = make(map[string]struct{})

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

func (p *ompEventParser) handleEvent(event map[string]any) {
	typ, _ := event["type"].(string)
	switch typ {
	case "message_update":
		p.rememberAssistant(event["message"])
		p.handleAssistantEvent(event["assistantMessageEvent"])
	case "message_end", "turn_end":
		p.rememberAssistant(event["message"])
		p.recordAssistantUsage(event["message"])
	case "agent_end":
		p.agentEnded = true
		p.rememberAgentEnd(event["messages"])
	}
}

func (p *ompEventParser) rememberAssistant(raw any) {
	msg, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if role, _ := msg["role"].(string); role != "assistant" {
		return
	}
	p.finalAssistant = msg
	if v, _ := msg["model"].(string); v != "" {
		p.model = v
	}
	if v, _ := msg["provider"].(string); v != "" {
		p.provider = v
	}

	if reason, _ := msg["stopReason"].(string); reason == "error" || reason == "aborted" {
		p.assistantError = firstStringField(msg, "errorMessage", "error", "message")
		if p.assistantError == "" {
			p.assistantError = fmt.Sprintf("stopReason=%s", reason)
		}
	} else {
		p.assistantError = ""
	}
}

func (p *ompEventParser) rememberAgentEnd(raw any) {
	messages, ok := raw.([]any)
	if !ok {
		return
	}

	total := TokenUsage{}
	seen := make(map[string]struct{})
	hasUsage := false
	for i, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		usageMap, ok := msg["usage"].(map[string]any)
		if !ok {
			continue
		}
		usage := usageFromMap(usageMap)
		if usageIsZero(usage) {
			continue
		}
		key := usageKey(msg)
		if key == "" {
			key = fmt.Sprintf("agent_end:%d", i)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		total = addUsage(total, usage)
		hasUsage = true
	}
	if hasUsage {
		p.usage = total
		p.seenUsage = make(map[string]struct{}, len(seen))
		for key := range seen {
			p.seenUsage[key] = struct{}{}
		}
	}

	for i := len(messages) - 1; i >= 0; i-- {
		if msg, ok := messages[i].(map[string]any); ok && msg["role"] == "assistant" {
			p.rememberAssistant(msg)
			return
		}
	}
}

func (p *ompEventParser) recordAssistantUsage(raw any) {
	msg, ok := raw.(map[string]any)
	if !ok || msg["role"] != "assistant" {
		return
	}
	usageMap, ok := msg["usage"].(map[string]any)
	if !ok {
		return
	}
	usage := usageFromMap(usageMap)
	if usageIsZero(usage) {
		return
	}
	key := usageKey(msg)
	if key == "" {
		encoded, err := json.Marshal([]any{msg["role"], msg["stopReason"], msg["content"], msg["usage"]})
		if err != nil {
			return
		}
		key = string(encoded)
	}
	if p.seenUsage == nil {
		p.seenUsage = make(map[string]struct{})
	}
	if _, ok := p.seenUsage[key]; ok {
		return
	}
	p.seenUsage[key] = struct{}{}
	p.usage = addUsage(p.usage, usage)
}

func (p *ompEventParser) handleAssistantEvent(raw any) {
	evt, ok := raw.(map[string]any)
	if !ok {
		return
	}
	idx := intField(evt, "contentIndex", "content_index")
	switch evt["type"] {
	case "text_delta":
		delta := firstStringField(evt, "delta", "text", "content")
		if delta == "" {
			return
		}
		p.streamText[idx] += delta
		if p.onChunk != nil {
			p.onChunk(delta)
		}
	case "text_end":
		text := firstStringField(evt, "text", "content")
		if text == "" {
			text = p.streamText[idx]
		}
		p.completeText[idx] = text
	}
}

// finalText returns the final assistant text, preferring (in order) the
// content of the last assistant message, the text_end-completed deltas, and
// finally the in-flight stream buffer.
func (p *ompEventParser) finalText() string {
	if text := strings.TrimSpace(textFromAssistantMessage(p.finalAssistant)); text != "" {
		return text
	}
	if text := strings.TrimSpace(joinByIndex(p.completeText)); text != "" {
		return text
	}
	return strings.TrimSpace(joinByIndex(p.streamText))
}

func (p *ompEventParser) failedResult() *Result {
	res := failedResult(p.usage)
	if res == nil && (p.model != "" || p.provider != "") {
		res = &Result{}
	}
	if res != nil {
		res.Model = p.model
		res.ModelProvider = p.provider
	}
	return res
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

func firstStringField(m map[string]any, names ...string) string {
	for _, n := range names {
		if v, ok := m[n].(string); ok {
			return v
		}
	}
	return ""
}

func intField(m map[string]any, names ...string) int {
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

func usageFromMap(usage map[string]any) TokenUsage {
	_, cacheCreationReported := usage["cacheWrite"]
	return TokenUsage{
		Reported:              len(usage) > 0,
		CacheCreationReported: cacheCreationReported,
		InputTokens:           intField(usage, "input") + intField(usage, "cacheRead") + intField(usage, "cacheWrite"),
		OutputTokens:          intField(usage, "output"),
		CacheReadTokens:       intField(usage, "cacheRead"),
		CacheCreationTokens:   intField(usage, "cacheWrite"),
	}
}

func addUsage(a, b TokenUsage) TokenUsage {
	return TokenUsage{
		Reported:              a.Reported || b.Reported,
		CacheCreationReported: a.CacheCreationReported || b.CacheCreationReported,
		InputTokens:           a.InputTokens + b.InputTokens,
		OutputTokens:          a.OutputTokens + b.OutputTokens,
		CacheReadTokens:       a.CacheReadTokens + b.CacheReadTokens,
		CacheCreationTokens:   a.CacheCreationTokens + b.CacheCreationTokens,
	}
}

func usageIsZero(usage TokenUsage) bool {
	return usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0
}

func usageKey(msg map[string]any) string {
	for _, name := range []string{"responseId", "id"} {
		if value, ok := msg[name].(string); ok && value != "" {
			return name + ":" + value
		}
	}
	return ""
}

// inlineSchemaPrompt appends the prompt-only structured-output contract: the
// schema is inlined and the final assistant text is parsed and validated
// against it afterwards.
func inlineSchemaPrompt(prompt string, schema json.RawMessage) string {
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
