package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func runGemini(args []string, promptReader io.Reader, scenario *Scenario) int {
	prompt, err := extractClaudePrompt(args, promptReader) // gemini args shape is similar enough to reuse this
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: gemini prompt: %v\n", err)
		return 1
	}
	logInvocation("gemini", prompt, args)

	action := scenario.Match(prompt)
	if err := applyAction(action); err != nil {
		return 1
	}

	enc := json.NewEncoder(os.Stdout)

	content := action.textOrDefault()
	if strings.Contains(prompt, "CRITICAL: You must output your final answer as a single structured JSON block") {
		content = fmt.Sprintf("%s\n```json\n%s\n```", content, action.structuredJSON())
	}

	_ = enc.Encode(map[string]any{
		"type":    "message",
		"role":    "assistant",
		"content": content,
	})

	_ = enc.Encode(map[string]any{
		"type":   "result",
		"status": "success",
		"stats": map[string]int{
			"input_tokens":  100,
			"output_tokens": 50,
		},
	})
	return 0
}
