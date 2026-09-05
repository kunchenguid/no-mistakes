package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
)

// fakePiEffortHandler scripts two review findings then a clean rereview. It
// exercises the native Pi adapter and durable fixer sessions without credentials.
func fakePiEffortHandler(args []string) {
	_, _ = io.Copy(io.Discard, os.Stdin)
	if slices.Contains(args, "--session") && os.Getenv("FAKE_PI_FAIL_RESUME") == "1" {
		fmt.Fprintln(os.Stderr, "fixture resume rejected")
		os.Exit(1)
	}
	output := map[string]any{"summary": "fixture fix"}
	if slices.Contains(args, "--no-session") {
		path := os.Getenv("FAKE_PI_REVIEW_COUNT")
		data, _ := os.ReadFile(path)
		n, _ := strconv.Atoi(string(data))
		n++
		if err := os.WriteFile(path, []byte(strconv.Itoa(n)), 0600); err != nil {
			panic(err)
		}
		findings := []map[string]any{}
		if n <= 2 {
			findings = append(findings, map[string]any{"id": fmt.Sprintf("f-%d", n), "severity": "error", "description": "fixture bug", "action": "auto-fix", "review_scope": "source"})
		}
		output = map[string]any{"summary": "fixture review", "findings": findings, "risk_level": "low", "risk_rationale": "fixture", "risk_scope": "source-or-external"}
	} else {
		fmt.Println(`{"type":"session","version":3,"id":"019ff2f3-5f31-744b-90b8-679074ff7686","timestamp":"2026-08-21T00:00:00.000Z"}`)
	}
	text, _ := json.Marshal(output)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": string(text)}}}})
}
