package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func runCodex(args []string, scenario *Scenario) int {
	prompt := extractCodexPrompt(args)
	logInvocation("codex", prompt, args)

	action := scenario.Match(prompt)
	if err := applyAction(action); err != nil {
		return 1
	}

	// Real codex constrains output to --output-schema, so the fake
	// mirrors that by trimming the scenario's catch-all structured map
	// to only fields declared in the schema. Otherwise no-mistakes'
	// schema validation rejects the extra fields the defaultScenario
	// carries to satisfy other steps (e.g. pr's title/body).
	if schemaPath := extractCodexOutputSchema(args); schemaPath != "" && action.Structured != nil {
		filtered, err := filterStructuredToSchema(action.Structured, schemaPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: codex schema filter: %v\n", err)
			return 1
		}
		action.Structured = filtered
	}

	// Replay recorded codex output if a fixture is available. no-mistakes
	// passes a schema file for structured calls, but Codex still surfaces
	// the final answer as agent_message text, so the fixture patches that
	// message body directly.
	flavour := "structured"
	if !action.hasStructuredOutput() && action.Text != "" {
		flavour = "plain"
	}
	if data, err := readFixtureFile(fixtureDir("codex"), flavour, ".jsonl"); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: codex fixture: %v\n", err)
		return 1
	} else if data != nil {
		patched, err := patchCodexFixture(data, action)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: codex patch: %v\n", err)
			return 1
		}
		os.Stdout.Write(patched)
		return 0
	}

	// Structured Codex output is still delivered as agent_message text.
	// Emit JSON there when requested, otherwise emit the human text.
	body := action.textOrDefault()
	if action.hasStructuredOutput() {
		body = string(action.structuredJSON())
	}

	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type": "agent_message",
			"text": body,
		},
	})
	_ = enc.Encode(map[string]any{
		"type": "turn.completed",
		"usage": map[string]int{
			"input_tokens":        100,
			"cached_input_tokens": 0,
			"output_tokens":       50,
		},
	})
	return 0
}

// patchCodexFixture rewrites the agent_message item's text body to
// match the scenario action. The wire envelope (thread.started,
// turn.started, item.completed shape, turn.completed.usage) stays
// real. no-mistakes parses JSON out of the agent_message text, so for
// structured responses we substitute the scenario JSON.
func patchCodexFixture(raw []byte, action Action) ([]byte, error) {
	body := action.textOrDefault()
	if action.hasStructuredOutput() {
		body = string(action.structuredJSON())
	}
	var out bytes.Buffer
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			out.WriteByte('\n')
			continue
		}
		var probe struct {
			Type string `json:"type"`
			Item *struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if err := json.Unmarshal(line, &probe); err != nil ||
			probe.Type != "item.completed" ||
			probe.Item == nil ||
			probe.Item.Type != "agent_message" {
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("parse item event: %w", err)
		}
		item, _ := event["item"].(map[string]any)
		if item != nil {
			item["text"] = body
			event["item"] = item
		}
		patched, err := json.Marshal(event)
		if err != nil {
			return nil, fmt.Errorf("marshal patched item: %w", err)
		}
		out.Write(patched)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// extractCodexOutputSchema returns the --output-schema value from the
// argv, supporting both `--output-schema path` and `--output-schema=path`.
// Returns "" when the flag is absent.
func extractCodexOutputSchema(args []string) string {
	for i, a := range args {
		switch {
		case a == "--output-schema" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "--output-schema="):
			return strings.TrimPrefix(a, "--output-schema=")
		}
	}
	return ""
}

// filterStructuredToSchema drops fields from structured that are not
// declared as properties on the top-level object schema at schemaPath.
// Real codex would not emit undeclared fields under --output-schema, so
// mirroring that behaviour keeps the fake consistent with no-mistakes'
// additionalProperties:false validation. schemaPath == "" is a no-op.
func filterStructuredToSchema(structured map[string]any, schemaPath string) (map[string]any, error) {
	if schemaPath == "" {
		return structured, nil
	}
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("read schema %s: %w", schemaPath, err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("parse schema %s: %w", schemaPath, err)
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		return structured, nil
	}
	filtered := make(map[string]any, len(properties))
	for key, value := range structured {
		if _, ok := properties[key]; ok {
			filtered[key] = value
		}
	}
	return filtered, nil
}

// extractCodexPrompt finds the prompt positional. Real codex argv is
// `codex exec [user-flags...] <prompt> --json [...]`. The prompt is the
// first non-flag, non-flag-value positional after "exec".
func extractCodexPrompt(args []string) string {
	flagsWithValues := map[string]bool{
		"-m": true, "--model": true,
		"--sandbox": true, "--ask-for-approval": true,
		"--config": true, "--profile": true,
		"--output-schema":    true,
		"--reasoning-effort": true, "--reasoning-summary": true,
		"-c": true, "--cd": true,
	}
	start := 0
	for i, a := range args {
		if a == "exec" {
			start = i + 1
			break
		}
	}
	for i := start; i < len(args); i++ {
		a := args[i]
		if flagsWithValues[a] {
			i++
			continue
		}
		if len(a) > 0 && a[0] == '-' {
			continue
		}
		return a
	}
	return ""
}
