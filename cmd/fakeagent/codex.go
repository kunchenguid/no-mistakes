package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

func runCodex(args []string, scenario *Scenario) int {
	prompt := extractCodexPrompt(args)
	logInvocation("codex", prompt, args)

	action := scenario.Match(prompt)
	if err := applyEdits(action.Edits); err != nil {
		return 1
	}

	// Replay recorded codex output if a fixture is available. no-mistakes
	// passes a schema file for structured calls, but Codex still surfaces
	// the final answer as agent_message text, so the fixture patches that
	// message body directly.
	flavour := "structured"
	if action.Structured == nil && action.Text != "" {
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
	if action.Structured != nil {
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
	if action.Structured != nil {
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
