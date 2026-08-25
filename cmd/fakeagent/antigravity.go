package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func runAgy(args []string, scenario *Scenario) int {
	prompt, err := extractAgyPrompt(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: agy prompt: %v\n", err)
		return 1
	}
	logInvocation("antigravity", prompt, args)

	action := scenario.Match(prompt)
	if err := applyAction(action); err != nil {
		return 1
	}

	// Fixture mode: replay the real agy wire envelope captured from a
	// live headless run, splicing scenario-driven content into the fields
	// no-mistakes parses (step_update text deltas, result response and
	// structured_output). The event ordering and field shapes stay exactly
	// what agy emits, so wire-format drift surfaces in e2e.
	flavour := "plain"
	if hasAgySchema(args) {
		flavour = "structured"
	}
	if data, err := readFixtureFile(fixtureDir("antigravity"), flavour, ".jsonl"); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: antigravity fixture: %v\n", err)
		return 1
	} else if data != nil {
		patched, err := patchAgyFixture(data, action)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: agy patch: %v\n", err)
			return 1
		}
		os.Stdout.Write(patched)
		return 0
	}

	// Synthetic fallback when no recorded fixture is available.
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{
		"event":           "init",
		"conversation_id": "fake-agy-session",
	})
	_ = enc.Encode(map[string]any{
		"event": "step_update",
		"step_update": map[string]any{
			"conversation_id": "fake-agy-session",
			"step_index":      0,
			"state":           "DONE",
			"step_type":       "agent_response",
			"text_delta":      action.textOrDefault(),
			"usage": map[string]any{
				"input_tokens":      100,
				"output_tokens":     50,
				"thinking_tokens":   8,
				"cache_read_tokens": 0,
				"total_tokens":      150,
			},
		},
	})
	result := map[string]any{
		"event": "result",
		"result": map[string]any{
			"conversation_id": "fake-agy-session",
			"status":          "SUCCESS",
			"response":        action.textOrDefault(),
			"num_turns":       1,
			"usage": map[string]any{
				"input_tokens":      100,
				"output_tokens":     50,
				"thinking_tokens":   8,
				"cache_read_tokens": 0,
				"total_tokens":      150,
			},
		},
	}
	if action.hasStructuredOutput() {
		result["result"].(map[string]any)["structured_output"] = json.RawMessage(action.structuredJSON())
	}
	_ = enc.Encode(result)
	return 0
}

// patchAgyFixture rewrites the agent_response text delta and the terminal
// result's response/structured_output to match the scenario action, leaving
// every other event byte-for-byte so the envelope stays a real agy trace.
func patchAgyFixture(raw []byte, action Action) ([]byte, error) {
	text := action.textOrDefault()
	structuredJSON := action.structuredJSON()
	var out bytes.Buffer
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			out.WriteByte('\n')
			continue
		}
		var probe struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		switch probe.Event {
		case "step_update":
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				return nil, fmt.Errorf("parse step_update event: %w", err)
			}
			step, _ := event["step_update"].(map[string]any)
			if step != nil && step["step_type"] == "agent_response" {
				step["text_delta"] = text
				event["step_update"] = step
				patched, err := json.Marshal(event)
				if err != nil {
					return nil, fmt.Errorf("marshal patched step_update: %w", err)
				}
				line = patched
			}
		case "result":
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				return nil, fmt.Errorf("parse result event: %w", err)
			}
			result, _ := event["result"].(map[string]any)
			if result != nil {
				result["response"] = text
				if action.hasStructuredOutput() {
					result["structured_output"] = json.RawMessage(structuredJSON)
				} else {
					delete(result, "structured_output")
				}
				event["result"] = result
				patched, err := json.Marshal(event)
				if err != nil {
					return nil, fmt.Errorf("marshal patched result: %w", err)
				}
				line = patched
			}
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func hasAgySchema(args []string) bool {
	for i, arg := range args {
		if arg == "--json-schema" && i < len(args)-1 {
			return true
		}
		if strings.HasPrefix(arg, "--json-schema=") {
			return true
		}
	}
	return false
}

// extractAgyPrompt mirrors agy's print-mode contract: --print (or -p /
// --prompt) is followed by the prompt text as the next argv element.
func extractAgyPrompt(args []string) (string, error) {
	for _, flag := range []string{"--print", "-p", "--prompt"} {
		if prompt := argAfter(args, flag); prompt != "" {
			return prompt, nil
		}
	}
	return "", fmt.Errorf("missing --print")
}
