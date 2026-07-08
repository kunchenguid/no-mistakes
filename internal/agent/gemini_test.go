package agent

import (
	"encoding/json"
	"testing"
)

func TestGeminiAgent_BuildArgs(t *testing.T) {
	ga := &geminiAgent{bin: "/usr/bin/gemini"}
	schema := json.RawMessage(`{"type":"object"}`)
	args := ga.buildArgs("do something", schema)

	expected := []string{
		"-p", "do something",
		"--verbose",
		"--output-format", "stream-json",
		"--json-schema", `{"type":"object"}`,
		"--model", "gemini-3.1-pro",
		"--dangerously-skip-permissions",
	}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestGeminiAgent_BuildArgs_UserSetModel(t *testing.T) {
	tests := [][]string{
		{"--model", "gemini-2.0-pro"},
		{"--model=gemini-2.0-pro"},
		{"-m", "gemini-1.5-flash"},
		{"-m=gemini-1.5-flash"},
	}
	for _, extra := range tests {
		ga := &geminiAgent{bin: "gemini", extraArgs: extra}
		args := ga.buildArgs("p", nil)

		hasDefault := false
		for _, a := range args {
			if a == "gemini-3.1-pro" {
				hasDefault = true
			}
		}
		// The custom extraArgs should provide one instance, and we should NOT add the default
		if hasDefault {
			t.Errorf("extra=%v expected no default gemini-3.1-pro, got args: %v", extra, args)
		}
	}
}

func TestGeminiAgent_BuildArgs_UserPermissionModeSuppressesDefault(t *testing.T) {
	tests := [][]string{
		{"--permission-mode", "acceptEdits"},
		{"--permission-mode=plan"},
		{"--dangerously-skip-permissions"},
	}
	for _, extra := range tests {
		ga := &geminiAgent{bin: "gemini", extraArgs: extra}
		args := ga.buildArgs("p", nil)

		dangerCount := 0
		for _, a := range args {
			if a == "--dangerously-skip-permissions" {
				dangerCount++
			}
		}
		if len(extra) == 1 && extra[0] == "--dangerously-skip-permissions" {
			if dangerCount != 1 {
				t.Errorf("extra=%v expected single --dangerously-skip-permissions, got %d: %v", extra, dangerCount, args)
			}
		} else if dangerCount != 0 {
			t.Errorf("extra=%v expected no default --dangerously-skip-permissions, got: %v", extra, args)
		}
	}
}
