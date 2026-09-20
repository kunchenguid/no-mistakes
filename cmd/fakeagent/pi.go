package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// runPi is a synthetic JSON-mode fixture for dispatch tests, not a recorded
// provider response. Session header + message_end + agent_end follow Pi's
// documented JSON stream and the adapter's existing wire regressions.
func runPi(args []string, input io.Reader, scenario *Scenario) int {
	data, err := io.ReadAll(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	prompt := string(data)
	logInvocation("pi", prompt, args)
	action := scenario.Match(prompt)
	if err := applyAction(action); err != nil {
		return 1
	}
	session := argAfter(args, "--session")
	if session == "" {
		session = "00000000-0000-4000-8000-000000000001"
	}
	provider, model, ok := strings.Cut(argAfter(args, "--model"), "/")
	if !ok {
		provider, model = "fake", "default"
	}
	msg := map[string]any{
		"role": "assistant", "model": model, "provider": provider, "stopReason": "stop",
		"content": []any{map[string]any{"type": "text", "text": string(action.structuredJSON())}},
		"usage":   map[string]int{"input": 100, "output": 50, "cacheRead": 0, "cacheWrite": 0},
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{"type": "session", "version": 3, "id": session})
	_ = enc.Encode(map[string]any{"type": "message_end", "message": msg})
	_ = enc.Encode(map[string]any{"type": "agent_end", "messages": []any{msg}})
	return 0
}
