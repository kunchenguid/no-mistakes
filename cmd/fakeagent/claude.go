package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func runClaude(args []string, promptReader io.Reader, scenario *Scenario) int {
	prompt, err := extractClaudePrompt(args, promptReader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: claude prompt: %v\n", err)
		return 1
	}
	logInvocation("claude", prompt, args)

	action := scenario.Match(prompt)
	if err := applyAction(action); err != nil {
		return 1
	}

	// Fixture mode: replay the real claude wire envelope captured by
	// recordfixture, but splice in scenario-driven content for the
	// fields no-mistakes parses (assistant text, result structured
	// output). The envelope (event ordering, field shapes, system
	// events, rate-limit events, etc.) stays exactly what real claude
	// emits, so wire-format drift surfaces here. The content stays
	// test-deterministic, so happy-path scenarios pass without
	// depending on whatever the live API happened to return when the
	// fixture was recorded.
	flavour := "plain"
	if hasClaudeSchema(args) {
		flavour = "structured"
	}
	if data, err := readFixtureFile(fixtureDir("claude"), flavour, ".jsonl"); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: claude fixture: %v\n", err)
		return 1
	} else if data != nil {
		patched, err := patchClaudeFixture(data, action)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: claude patch: %v\n", err)
			return 1
		}
		os.Stdout.Write(patched)
		return 0
	}

	enc := json.NewEncoder(os.Stdout)

	// Match the real claude CLI's JSONL stream-json format. Real claude
	// emits init + assistant + result events; no-mistakes' parser ignores
	// any type it doesn't know, so init is optional. We emit one assistant
	// event with the text content + a result event with the structured
	// output and final usage.
	_ = enc.Encode(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"usage": map[string]int{
				"input_tokens":  100,
				"output_tokens": 50,
			},
			"content": []any{
				map[string]any{"type": "text", "text": action.textOrDefault()},
			},
		},
	})
	_ = enc.Encode(map[string]any{
		"type":              "result",
		"subtype":           "success",
		"is_error":          false,
		"structured_output": json.RawMessage(action.structuredJSON()),
		"usage": map[string]int{
			"input_tokens":  100,
			"output_tokens": 50,
		},
	})
	return 0
}

// patchClaudeFixture rewrites the result event's structured_output to
// match the scenario action, leaving every other event byte-for-byte.
// The result event is identified as the one whose top-level "type" is
// "result" — there's exactly one per session in stream-json output.
//
// Why we don't just emit the recorded structured_output: the recorded
// payload reflects whatever the live model returned at recording time,
// which may not satisfy the schemas every step in the pipeline expects
// (e.g. document.go's unmarshalRequiredFindings requires "summary").
// Patching keeps the wire shape real but the content predictable.
func patchClaudeFixture(raw []byte, action Action) ([]byte, error) {
	if action.Structured == nil && action.StructuredRaw == "" {
		return raw, nil
	}
	structuredJSON := action.structuredJSON()
	text := action.textOrDefault()
	var out bytes.Buffer
	seenAssistant := false
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			out.WriteByte('\n')
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		if probe.Type == "assistant" {
			seenAssistant = true
			patched, err := patchClaudeAssistantEvent(line, text)
			if err != nil {
				return nil, err
			}
			out.Write(patched)
			out.WriteByte('\n')
			continue
		}
		if probe.Type != "result" {
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		if !seenAssistant {
			assistant, err := patchClaudeAssistantEvent([]byte(`{"type":"assistant","message":{"content":[]}}`), text)
			if err != nil {
				return nil, err
			}
			out.Write(assistant)
			out.WriteByte('\n')
			seenAssistant = true
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("parse result event: %w", err)
		}
		event["structured_output"] = json.RawMessage(structuredJSON)
		event["result"] = text
		patched, err := json.Marshal(event)
		if err != nil {
			return nil, fmt.Errorf("marshal patched result: %w", err)
		}
		out.Write(patched)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func patchClaudeAssistantEvent(line []byte, text string) ([]byte, error) {
	var event map[string]any
	if err := json.Unmarshal(line, &event); err != nil {
		return nil, fmt.Errorf("parse assistant event: %w", err)
	}
	message, _ := event["message"].(map[string]any)
	if message != nil {
		message["content"] = patchClaudeAssistantContent(message["content"], text)
		event["message"] = message
	}
	patched, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal patched assistant: %w", err)
	}
	return patched, nil
}

func patchClaudeAssistantContent(raw any, text string) []any {
	items, _ := raw.([]any)
	if len(items) == 0 {
		return []any{map[string]any{"type": "text", "text": text}}
	}
	patched := make([]any, 0, len(items))
	replaced := false
	for _, item := range items {
		content, ok := item.(map[string]any)
		if !ok {
			patched = append(patched, item)
			continue
		}
		if content["type"] != "text" {
			patched = append(patched, content)
			continue
		}
		if replaced {
			continue
		}
		copyItem := make(map[string]any, len(content))
		for k, v := range content {
			copyItem[k] = v
		}
		copyItem["text"] = text
		patched = append(patched, copyItem)
		replaced = true
	}
	if !replaced {
		patched = append(patched, map[string]any{"type": "text", "text": text})
	}
	return patched
}

func hasClaudeSchema(args []string) bool {
	for _, a := range args {
		if a == "--json-schema" {
			return true
		}
	}
	return false
}

// extractClaudePrompt mirrors Claude Code's documented print-mode contract:
// -p selects non-interactive mode and the text user prompt is read to EOF from
// stdin. Keeping the e2e fake on this shape ensures prompts cannot regress into
// process argv unnoticed.
func extractClaudePrompt(args []string, promptReader io.Reader) (string, error) {
	foundPrint := false
	for _, arg := range args {
		if arg == "-p" || arg == "--print" {
			foundPrint = true
			break
		}
	}
	if !foundPrint {
		return "", fmt.Errorf("missing -p")
	}
	prompt, err := io.ReadAll(promptReader)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(prompt), nil
}
