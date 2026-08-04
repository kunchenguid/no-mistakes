package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobal_AgentModelByPurpose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `agent: [codex, claude]
agent_model_by_purpose:
  review:
    claude: claude-opus-5
    codex: gpt-5.4
  review-fix:
    claude: claude-sonnet-4-5
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	global, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal() error = %v", err)
	}
	if got := global.AgentModelByPurpose[types.AgentPurposeReview]["claude"]; got != "claude-opus-5" {
		t.Fatalf("review claude model = %q, want claude-opus-5", got)
	}
	if got := global.AgentModelByPurpose[types.AgentPurposeReview]["codex"]; got != "gpt-5.4" {
		t.Fatalf("review codex model = %q, want gpt-5.4", got)
	}

	merged := Merge(global, &RepoConfig{})
	if got := merged.AgentModelsFor(types.AgentClaude)[types.AgentPurposeReviewFix]; got != "claude-sonnet-4-5" {
		t.Fatalf("merged review-fix claude model = %q, want claude-sonnet-4-5", got)
	}
}

func TestLoadGlobal_AgentModelByPurposeRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "unknown purpose",
			yaml: "agent_model_by_purpose:\n  deploy:\n    claude: opus\n",
			want: "deploy",
		},
		{
			name: "unsupported adapter",
			yaml: "agent_model_by_purpose:\n  review:\n    opencode: openai/gpt-5\n",
			want: "opencode",
		},
		{
			name: "blank model",
			yaml: "agent_model_by_purpose:\n  review:\n    claude: \"  \"\n",
			want: "must not be empty",
		},
		{
			name: "model with whitespace",
			yaml: "agent_model_by_purpose:\n  review:\n    codex: \"gpt 5\"\n",
			want: "must not contain whitespace",
		},
		{
			name: "model with control character",
			yaml: "agent_model_by_purpose:\n  review:\n    claude: \"opus\\u0007\"\n",
			want: "control characters",
		},
		{
			name: "empty purpose route",
			yaml: "agent_model_by_purpose:\n  review: {}\n",
			want: "at least one agent",
		},
		{
			name: "model name too long",
			yaml: "agent_model_by_purpose:\n  review:\n    claude: " + strings.Repeat("x", maxAgentModelNameBytes+1) + "\n",
			want: "must not exceed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadGlobal(path)
			if err == nil {
				t.Fatal("LoadGlobal() accepted invalid agent_model_by_purpose")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadGlobal() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLoadGlobal_AgentModelByPurposeLogsModelArgPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `agent_args_override:
  claude: [--model, global-model]
  codex: [--model=global-model]
agent_model_by_purpose:
  review:
    claude: review-model
    codex: review-model
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	if _, err := LoadGlobal(path); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"agent_model_by_purpose overrides agent_args_override model selection",
		"purpose=review",
		"agent=claude",
		"agent=codex",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("precedence log missing %q:\n%s", want, logs.String())
		}
	}
	for _, model := range []string{"global-model", "review-model"} {
		if strings.Contains(logs.String(), model) {
			t.Fatalf("precedence log exposed model value %q:\n%s", model, logs.String())
		}
	}
}

func TestAgentModelByPurposeIsGlobalOnly(t *testing.T) {
	_, err := LoadRepoFromBytes([]byte("agent_model_by_purpose:\n  review:\n    claude: opus\n"))
	if err == nil {
		t.Fatal("repo config accepted global-only agent_model_by_purpose")
	}
	if !strings.Contains(err.Error(), "agent_model_by_purpose") {
		t.Fatalf("repo config error = %v, want field name", err)
	}
}

func TestAgentModelByPurposeDefaultsOff(t *testing.T) {
	global := DefaultGlobalConfig()
	if len(global.AgentModelByPurpose) != 0 {
		t.Fatalf("default AgentModelByPurpose = %v, want empty", global.AgentModelByPurpose)
	}
	merged := Merge(global, &RepoConfig{})
	if got := merged.AgentModelsFor(types.AgentClaude); len(got) != 0 {
		t.Fatalf("default Claude purpose models = %v, want empty", got)
	}
}
