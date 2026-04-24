package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Agent is the interface for running AI agent tasks.
type Agent interface {
	Name() string
	Run(ctx context.Context, opts RunOpts) (*Result, error)
	Close() error
}

// RunOpts configures a single agent invocation.
type RunOpts struct {
	Prompt     string
	CWD        string
	JSONSchema json.RawMessage   // structured output schema (optional)
	OnChunk    func(text string) // streaming text callback (optional)
}

// Result holds the output of an agent invocation.
type Result struct {
	Output json.RawMessage // structured output matching JSONSchema
	Text   string          // raw text output
	Usage  TokenUsage
}

// TokenUsage tracks token consumption for an agent invocation.
type TokenUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

func finalizeTextResult(agentName, text string, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	if text == "" {
		return nil, fmt.Errorf("%s returned no text output", agentName)
	}
	if len(schema) == 0 {
		return &Result{Text: text, Usage: usage}, nil
	}

	output, err := parseStructuredTextOutput(text, schema)
	if err != nil {
		return nil, fmt.Errorf("%s output parse: %w", agentName, err)
	}

	return &Result{Output: output, Text: text, Usage: usage}, nil
}

func parseStructuredTextOutput(text string, schema json.RawMessage) (json.RawMessage, error) {
	validationSchema, err := textValidationSchema(schema)
	if err != nil {
		return nil, err
	}

	output, rawErr := parseStructuredCandidate([]byte(text), validationSchema)
	if rawErr == nil {
		return output, nil
	}

	candidates := fencedJSONCandidates(text)
	var parsed []json.RawMessage
	var candidateErr error
	for _, candidate := range candidates {
		fenced, err := parseStructuredCandidate([]byte(candidate), validationSchema)
		if err == nil {
			parsed = append(parsed, fenced)
			continue
		}
		if candidateErr == nil {
			candidateErr = err
		}
	}
	switch len(parsed) {
	case 0:
	case 1:
		return parsed[0], nil
	default:
		return nil, fmt.Errorf("multiple JSON code fences found in output")
	}

	if bare, err := lastBareJSONObject(text, validationSchema); err == nil && bare != nil {
		return bare, nil
	} else if candidateErr == nil && err != nil {
		candidateErr = err
	}

	if candidateErr != nil {
		return nil, candidateErr
	}
	return nil, rawErr
}

func textValidationSchema(schema json.RawMessage) (json.RawMessage, error) {
	if len(schema) == 0 {
		return nil, nil
	}

	var value any
	if err := json.Unmarshal(schema, &value); err != nil {
		return nil, err
	}
	allowOptionalSchemaNulls(value)
	return json.Marshal(value)
}

// fencedJSONCandidates extracts JSON bodies from ```json ... ``` fences.
// Fence markers may appear anywhere in the text, including glued to the end
// of a preceding line (e.g. "...behavior.```json"), which is a shape real
// codex/GPT-5 output regularly produces.
func fencedJSONCandidates(text string) []string {
	var candidates []string
	rest := text
	for {
		start := indexJSONFenceOpen(rest)
		if start < 0 {
			return candidates
		}
		body := rest[start:]
		end, next := indexJSONFenceClose(body)
		if end < 0 {
			return candidates
		}
		candidates = append(candidates, body[:end])
		rest = body[next:]
	}
}

// indexJSONFenceOpen returns the byte offset of the content immediately
// following an opening ```json fence (the char after the info line's
// newline), or -1 if no opener exists.
func indexJSONFenceOpen(text string) int {
	for searchStart := 0; searchStart < len(text); {
		i := strings.Index(text[searchStart:], "```")
		if i < 0 {
			return -1
		}
		i += searchStart
		contentStart, info := fenceContentStart(text, i)
		if strings.EqualFold(strings.TrimSpace(info), "json") {
			return contentStart
		}
		next := skipFenceBlock(text[contentStart:])
		if next < 0 {
			return -1
		}
		searchStart = contentStart + next
	}
	return -1
}

func fenceContentStart(text string, fenceStart int) (int, string) {
	after := text[fenceStart+3:]
	lineEnd := strings.IndexByte(after, '\n')
	if lineEnd < 0 {
		return fenceStart + 3 + len(after), after
	}
	return fenceStart + 3 + lineEnd + 1, after[:lineEnd]
}

func skipFenceBlock(text string) int {
	depth := 1
	for lineStart := 0; lineStart < len(text); {
		lineEnd := strings.IndexByte(text[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(text)
		} else {
			lineEnd += lineStart
		}
		line := text[lineStart:lineEnd]
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "```") {
			if strings.TrimSpace(trimmed[3:]) == "" {
				depth--
				if depth == 0 {
					return lineStart + (len(line) - len(trimmed)) + 3
				}
			} else {
				depth++
			}
		}
		if lineEnd == len(text) {
			break
		}
		lineStart = lineEnd + 1
	}
	return -1
}

func indexJSONFenceClose(text string) (int, int) {
	for lineStart := 0; lineStart < len(text); {
		lineEnd := strings.IndexByte(text[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(text)
		} else {
			lineEnd += lineStart
		}
		line := text[lineStart:lineEnd]
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "```") {
			indent := len(line) - len(trimmed)
			return lineStart, lineStart + indent + 3
		}
		if lineEnd == len(text) {
			break
		}
		lineStart = lineEnd + 1
	}
	trimmed := strings.TrimRight(text, " \t\r\n")
	if strings.HasSuffix(trimmed, "```") {
		return len(trimmed) - 3, len(trimmed)
	}
	return -1, -1
}

// lastBareJSONObject scans text for balanced {...} substrings that parse
// as JSON and returns the last one found. This handles models that emit
// reasoning prose followed by a raw JSON answer, with no code fence.
func lastBareJSONObject(text string, schema json.RawMessage) (json.RawMessage, error) {
	var last json.RawMessage
	var lastErr error
	for i := 0; i < len(text); i++ {
		if strings.HasPrefix(text[i:], "```") {
			contentStart, _ := fenceContentStart(text, i)
			next := skipFenceBlock(text[contentStart:])
			if next < 0 {
				break
			}
			i = contentStart + next - 1
			continue
		}
		if text[i] != '{' {
			continue
		}
		end, ok := scanBalancedObject(text, i)
		if !ok {
			continue
		}
		candidate := text[i:end]
		obj, err := parseStructuredCandidate([]byte(candidate), schema)
		if err == nil {
			last = obj
			lastErr = nil
		} else if lastErr == nil {
			lastErr = err
		}
		i = end - 1
	}
	if last != nil {
		return last, nil
	}
	return nil, lastErr
}

// scanBalancedObject returns the exclusive end index of a brace-balanced
// substring starting at text[start] == '{', or (0, false) if no balanced
// closing brace exists. It respects JSON string literals so braces inside
// strings do not affect the depth count.
func scanBalancedObject(text string, start int) (int, bool) {
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func parseStructuredCandidate(candidate, schema []byte) (json.RawMessage, error) {
	var output json.RawMessage
	if err := json.Unmarshal(candidate, &output); err != nil {
		return nil, err
	}
	if err := validateStructuredOutput(output, schema); err != nil {
		return nil, err
	}
	return output, nil
}

func validateStructuredOutput(output, schema json.RawMessage) error {
	if len(schema) == 0 {
		return nil
	}

	var parsedSchema any
	if err := json.Unmarshal(schema, &parsedSchema); err != nil {
		return err
	}

	value, err := decodeJSONValue(output)
	if err != nil {
		return err
	}

	if err := validateJSONValue(value, parsedSchema, ""); err != nil {
		return fmt.Errorf("JSON output %w", err)
	}
	return nil
}

func allowOptionalSchemaNulls(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}

	required := requiredSet(schema)
	if properties, ok := schema["properties"].(map[string]any); ok {
		for name, property := range properties {
			allowOptionalSchemaNulls(property)
			if !required[name] {
				allowSchemaNull(property)
			}
		}
	}
	if items, ok := schema["items"]; ok {
		allowOptionalSchemaNulls(items)
	}
}

func decodeJSONValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return value, nil
}

func validateJSONValue(value, schema any, path string) error {
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		return nil
	}

	if enum, ok := schemaMap["enum"].([]any); ok && !matchesEnum(value, enum) {
		return fmt.Errorf("%smust match one of the allowed values", formatJSONPath(path))
	}

	if types, ok := schemaTypes(schemaMap); ok && !matchesAnyType(value, types) {
		return fmt.Errorf("%smust be %s", formatJSONPath(path), strings.Join(types, " or "))
	}

	if object, ok := value.(map[string]any); ok {
		if err := validateJSONObject(object, schemaMap, path); err != nil {
			return err
		}
	}
	if array, ok := value.([]any); ok {
		if err := validateJSONArray(array, schemaMap, path); err != nil {
			return err
		}
	}
	return nil
}

func validateJSONObject(object map[string]any, schema map[string]any, path string) error {
	required := stringSlice(schema["required"])
	for _, key := range required {
		if _, ok := object[key]; !ok {
			return fmt.Errorf("%smissing required field %q", formatJSONPath(path), key)
		}
	}

	properties, _ := schema["properties"].(map[string]any)
	if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
		for key := range object {
			if _, ok := properties[key]; !ok {
				return fmt.Errorf("%scontains unknown field %q", formatJSONPath(path), key)
			}
		}
	}

	for key, propSchema := range properties {
		child, ok := object[key]
		if !ok {
			continue
		}
		if err := validateJSONValue(child, propSchema, joinJSONPath(path, key)); err != nil {
			return err
		}
	}
	return nil
}

func validateJSONArray(array []any, schema map[string]any, path string) error {
	itemsSchema, ok := schema["items"]
	if !ok {
		return nil
	}
	for i, item := range array {
		if err := validateJSONValue(item, itemsSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func schemaTypes(schema map[string]any) ([]string, bool) {
	switch raw := schema["type"].(type) {
	case string:
		return []string{raw}, true
	case []any:
		types := stringSlice(raw)
		return types, len(types) > 0
	default:
		return nil, false
	}
}

func stringSlice(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		if single, ok := raw.([]string); ok {
			return single
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		str, ok := item.(string)
		if !ok {
			continue
		}
		out = append(out, str)
	}
	return out
}

func matchesAnyType(value any, types []string) bool {
	for _, typ := range types {
		if matchesType(value, typ) {
			return true
		}
	}
	return false
}

func matchesType(value any, typ string) bool {
	switch typ {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		return ok && isJSONInteger(number)
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}

func isJSONInteger(number json.Number) bool {
	_, err := number.Int64()
	return err == nil
}

func matchesEnum(value any, allowed []any) bool {
	valueJSON, err := json.Marshal(value)
	if err != nil {
		return false
	}
	for _, candidate := range allowed {
		candidateJSON, err := json.Marshal(candidate)
		if err != nil {
			continue
		}
		if bytes.Equal(valueJSON, candidateJSON) {
			return true
		}
	}
	return false
}

func joinJSONPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func formatJSONPath(path string) string {
	if path == "" {
		return ""
	}
	return path + " "
}

// Total returns input + output tokens (the billing-relevant total).
func (u TokenUsage) Total() int {
	return u.InputTokens + u.OutputTokens
}

// Add accumulates another usage into this one.
func (u *TokenUsage) Add(other TokenUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheCreationTokens += other.CacheCreationTokens
}

// New creates an agent by name with the given binary path. extraArgs are user
// CLI flags (from agent_args_override in the global config) that the agent
// injects into the underlying tool's argv ahead of no-mistakes' managed flags.
func New(name types.AgentName, bin string, extraArgs []string) (Agent, error) {
	switch name {
	case types.AgentClaude:
		return &claudeAgent{bin: bin, extraArgs: extraArgs}, nil
	case types.AgentCodex:
		return &codexAgent{bin: bin, extraArgs: extraArgs}, nil
	case types.AgentRovoDev:
		return &rovodevAgent{bin: bin, extraArgs: extraArgs}, nil
	case types.AgentOpenCode:
		return &opencodeAgent{bin: bin, extraArgs: extraArgs}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q; valid options: auto, claude, codex, rovodev, opencode (set 'agent' in ~/.no-mistakes/config.yaml)", name)
	}
}

// NewNoop returns an agent that does nothing. Used for demo mode where
// mock steps handle all logic without calling a real agent.
func NewNoop() Agent { return &noopAgent{} }

type noopAgent struct{}

func (n *noopAgent) Name() string                                      { return "noop" }
func (n *noopAgent) Run(_ context.Context, _ RunOpts) (*Result, error) { return &Result{}, nil }
func (n *noopAgent) Close() error                                      { return nil }
