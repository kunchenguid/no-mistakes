package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobal_AgentArgsOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `agent: claude
agent_args_override:
  claude:
    - --permission-mode
    - acceptEdits
  codex:
    - -m
    - gpt-5.4
    - --full-auto
  rovodev:
    - --profile
    - work
  opencode:
    - --model
    - gpt-5
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cases := map[string][]string{
		"claude":   {"--permission-mode", "acceptEdits"},
		"codex":    {"-m", "gpt-5.4", "--full-auto"},
		"rovodev":  {"--profile", "work"},
		"opencode": {"--model", "gpt-5"},
	}
	for agent, want := range cases {
		got := cfg.AgentArgsOverride[agent]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("agent_args_override[%q] = %v, want %v", agent, got, want)
		}
	}
}

func TestLoadGlobal_AgentArgsOverride_UnknownAgentRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `agent_args_override:
  gpt5:
    - --model
    - foo
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected error for unknown agent name in agent_args_override")
	}
	if !strings.Contains(err.Error(), "gpt5") {
		t.Errorf("expected error to mention unknown agent %q, got: %v", "gpt5", err)
	}
}

func TestLoadGlobal_AgentArgsOverride_ReservedArgsRejected(t *testing.T) {
	tests := []struct {
		agent string
		arg   string
	}{
		{"claude", "-p"},
		{"claude", "--print"},
		{"claude", "--verbose"},
		{"claude", "--output-format"},
		{"claude", "--output-format=stream-json"},
		{"claude", "--json-schema"},
		{"codex", "exec"},
		{"codex", "--json"},
		{"codex", "--color"},
		{"codex", "--color=never"},
		{"rovodev", "rovodev"},
		{"rovodev", "serve"},
		{"rovodev", "--disable-session-token"},
		{"opencode", "serve"},
		{"opencode", "--hostname"},
		{"opencode", "--port"},
		{"opencode", "--print-logs"},
	}
	for _, tt := range tests {
		t.Run(tt.agent+"_"+tt.arg, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			data := "agent_args_override:\n  " + tt.agent + ":\n    - " + tt.arg + "\n"
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadGlobal(path)
			if err == nil {
				t.Fatalf("expected error for reserved arg %q on agent %q", tt.arg, tt.agent)
			}
			if !strings.Contains(err.Error(), "managed by no-mistakes") {
				t.Errorf("error should mention 'managed by no-mistakes', got: %v", err)
			}
		})
	}
}

func TestLoadGlobal_AgentArgsOverride_EmptyArgRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `agent_args_override:
  claude:
    - ""
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected error for empty arg")
	}
}

func TestAgentArgs_ReturnsEmptyWhenUnset(t *testing.T) {
	cfg := &Config{Agent: types.AgentClaude}
	if got := cfg.AgentArgs(); len(got) != 0 {
		t.Errorf("AgentArgs() = %v, want empty", got)
	}
}

func TestAgentArgs_ReturnsExtrasForConfiguredAgent(t *testing.T) {
	cfg := &Config{
		Agent: types.AgentClaude,
		AgentArgsOverride: map[string][]string{
			"claude":  {"--permission-mode", "acceptEdits"},
			"codex":   {"-m", "gpt-5.4"},
			"unused":  {"whatever"},
			"rovodev": {"--profile", "work"},
		},
	}
	want := []string{"--permission-mode", "acceptEdits"}
	if got := cfg.AgentArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("AgentArgs() = %v, want %v", got, want)
	}
}

func TestMerge_PreservesAgentArgsOverride(t *testing.T) {
	global := &GlobalConfig{
		Agent: types.AgentClaude,
		AgentArgsOverride: map[string][]string{
			"claude": {"--permission-mode", "acceptEdits"},
		},
	}
	repo := &RepoConfig{}
	cfg := Merge(global, repo)
	if got := cfg.AgentArgsOverride["claude"]; !reflect.DeepEqual(got, []string{"--permission-mode", "acceptEdits"}) {
		t.Errorf("merged AgentArgsOverride[claude] = %v", got)
	}
}
