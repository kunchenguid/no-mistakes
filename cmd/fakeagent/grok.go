package main

import (
	"fmt"
	"os"
	"strings"
)

func runGrok(args []string, scenario *Scenario) int {
	prompt, err := extractGrokPrompt(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: grok prompt: %v\n", err)
		return 1
	}
	logInvocation("grok", prompt, args)

	action := scenario.Match(prompt)
	if err := applyAction(action); err != nil {
		return 1
	}

	body := action.textOrDefault()
	if action.hasStructuredOutput() {
		body = fmt.Sprintf("```json\n%s\n```", action.structuredJSON())
	}

	fmt.Fprint(os.Stdout, body)
	return 0
}

// extractGrokPrompt reads the prompt from --prompt-file. Real grok argv is
// `grok [user-flags...] --prompt-file <path> --cwd <dir> ...`.
func extractGrokPrompt(args []string) (string, error) {
	path := extractGrokPromptFile(args)
	if path == "" {
		return "", fmt.Errorf("missing --prompt-file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read prompt file %q: %w", path, err)
	}
	return string(data), nil
}

func extractGrokPromptFile(args []string) string {
	for i, a := range args {
		switch {
		case a == "--prompt-file" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "--prompt-file="):
			return strings.TrimPrefix(a, "--prompt-file=")
		}
	}
	return ""
}
