package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func writeGlobalConfig(t *testing.T, data string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadGlobal_AgentArgsOverridePerStep(t *testing.T) {
	path := writeGlobalConfig(t, `agent_args_override:
  codex:
    - -m
    - gpt-5.4
agent_args_override_per_step:
  review:
    codex:
      - -m
      - gpt-5.4
      - -c
      - model_reasoning_effort="high"
  document:
    codex:
      - -m
      - gpt-5.4-mini
`)
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	merged := Merge(cfg, &RepoConfig{})

	want := map[string][]string{"codex": {"-m", "gpt-5.4", "-c", `model_reasoning_effort="high"`}}
	if got := merged.AgentArgsForStep(types.StepReview); !reflect.DeepEqual(got, want) {
		t.Errorf("AgentArgsForStep(review) = %v, want %v", got, want)
	}
	if got := merged.AgentArgsForStep(types.StepDocument); !reflect.DeepEqual(got, map[string][]string{"codex": {"-m", "gpt-5.4-mini"}}) {
		t.Errorf("AgentArgsForStep(document) = %v", got)
	}
	// A step with no profile falls back to the global override, which the
	// adapters keep using when no per-step entry names them.
	if got := merged.AgentArgsForStep(types.StepTest); got != nil {
		t.Errorf("AgentArgsForStep(test) = %v, want nil so the global args apply", got)
	}
	if got := merged.AgentArgsFor(types.AgentCodex); !reflect.DeepEqual(got, []string{"-m", "gpt-5.4"}) {
		t.Errorf("global AgentArgsFor(codex) = %v, want the global override untouched", got)
	}
}

func TestAgentArgsForStep_NilConfigAndMissingMap(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.AgentArgsForStep(types.StepReview); got != nil {
		t.Errorf("nil config AgentArgsForStep = %v, want nil", got)
	}
	if got := (&Config{}).AgentArgsForStep(types.StepReview); got != nil {
		t.Errorf("empty config AgentArgsForStep = %v, want nil", got)
	}
}

func TestAgentArgsForStep_ReturnsCopy(t *testing.T) {
	cfg := &Config{AgentArgsOverrideStep: map[string]map[string][]string{
		"review": {"codex": {"-m", "gpt-5.4"}},
	}}
	got := cfg.AgentArgsForStep(types.StepReview)
	got["codex"][0] = "mutated"
	got["claude"] = []string{"injected"}
	if cfg.AgentArgsOverrideStep["review"]["codex"][0] != "-m" {
		t.Error("AgentArgsForStep must not alias the config's arg slices")
	}
	if _, ok := cfg.AgentArgsOverrideStep["review"]["claude"]; ok {
		t.Error("AgentArgsForStep must not alias the config's map")
	}
}

func TestLoadGlobal_AgentArgsOverridePerStep_Rejects(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name:    "unknown step",
			data:    "agent_args_override_per_step:\n  deploy:\n    codex:\n      - -m\n",
			wantErr: "invalid step name",
		},
		{
			name:    "unknown agent",
			data:    "agent_args_override_per_step:\n  review:\n    gpt:\n      - -m\n",
			wantErr: "invalid agent name",
		},
		{
			// Server-backed adapters fix their argv when the per-run server
			// starts, so a per-step entry could never take effect.
			name:    "server-backed agent",
			data:    "agent_args_override_per_step:\n  review:\n    opencode:\n      - --model\n",
			wantErr: "invalid agent name",
		},
		{
			name:    "reserved flag",
			data:    "agent_args_override_per_step:\n  review:\n    codex:\n      - --json\n",
			wantErr: "managed by no-mistakes",
		},
		{
			name:    "empty arg",
			data:    "agent_args_override_per_step:\n  review:\n    codex:\n      - \"  \"\n",
			wantErr: "empty arg",
		},
		{
			// The daemon verifies project-settings suppression once per run
			// against the run-level args; a per-step profile must not be able
			// to re-open that surface afterwards.
			name:    "codex project doc pin",
			data:    "agent_args_override_per_step:\n  review:\n    codex:\n      - -c\n      - project_doc_max_bytes=8192\n",
			wantErr: "project-settings suppression",
		},
		{
			name:    "claude setting sources pin",
			data:    "agent_args_override_per_step:\n  review:\n    claude:\n      - --setting-sources\n      - project\n",
			wantErr: "project-settings suppression",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadGlobal(writeGlobalConfig(t, tt.data))
			if err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestMerge_PreservesAgentArgsOverridePerStep(t *testing.T) {
	global := &GlobalConfig{AgentArgsOverrideStep: map[string]map[string][]string{
		"lint": {"claude": {"--model", "haiku"}},
	}}
	cfg := Merge(global, &RepoConfig{})
	if got := cfg.AgentArgsForStep(types.StepLint); !reflect.DeepEqual(got, map[string][]string{"claude": {"--model", "haiku"}}) {
		t.Errorf("merged AgentArgsForStep(lint) = %v", got)
	}
}
