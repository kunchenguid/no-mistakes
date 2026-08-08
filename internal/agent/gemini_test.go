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
		"-p", "do something\n\nCRITICAL: You must output your final answer as a single structured JSON block. Wrap your JSON in standard markdown fences (```json ... ```) so it can be extracted. It must strictly match this schema:\n```json\n{\"type\":\"object\"}\n```",
		"--output-format", "stream-json",
		"--model", "gemini-3.1-pro-preview-customtools",
		"-y",
		"--no-sandbox",
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
			if a == "gemini-3.1-pro-preview-customtools" {
				hasDefault = true
			}
		}
		// The custom extraArgs should provide one instance, and we should NOT add the default
		if hasDefault {
			t.Errorf("extra=%v expected no default gemini-3.1-pro-preview-customtools, got args: %v", extra, args)
		}
	}
}

func TestGeminiAgent_BuildArgs_UserPermissionModeSuppressesDefault(t *testing.T) {
	tests := [][]string{
		{"-y"},
		{"--no-sandbox"},
	}
	for _, extra := range tests {
		ga := &geminiAgent{bin: "gemini", extraArgs: extra}
		args := ga.buildArgs("p", nil)

		dangerCount := 0
		for _, a := range args {
			if a == "-y" || a == "--no-sandbox" {
				dangerCount++
			}
		}
		if dangerCount != 1 {
			t.Errorf("extra=%v expected no default permission flags added since user provided them, got: %v", extra, args)
		}
	}
}
