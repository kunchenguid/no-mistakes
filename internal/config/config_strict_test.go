package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func writeGlobalConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func writeRepoConfig(t *testing.T, data string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write repo config: %v", err)
	}
	return dir
}

func TestLoadGlobal_RejectsUnknownTopLevelKey(t *testing.T) {
	cases := map[string]string{
		"citimeout typo":     `citimeout: "4h"`,
		"agent_type typo":    `agent_type: claud`,
		"commands_top_level": `commands: { tst: "go test" }`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeGlobalConfig(t, data)
			_, err := LoadGlobal(path)
			if err == nil {
				t.Fatalf("expected error for unknown key, got nil")
			}
			if !strings.Contains(err.Error(), "parse global config") {
				t.Errorf("error should reference parse failure, got: %v", err)
			}
		})
	}
}

func TestLoadGlobal_RejectsUnknownNestedKey(t *testing.T) {
	cases := map[string]string{
		"auto_fix.babysit_typo": `auto_fix: { rebase: 3 }` + "\n" + `auto_fix: { rebas: 3 }`,
		"intent.thresh":         `intent: { thresh: 0.2 }`,
		"auto_fix_unknown":      `auto_fix: { rebse: 3 }`,
		"test_evidence_unknown": `test: { evidence: { dirr: foo } }`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeGlobalConfig(t, data)
			_, err := LoadGlobal(path)
			if err == nil {
				t.Fatalf("expected error for unknown nested key, got nil")
			}
			if !strings.Contains(err.Error(), "parse global config") {
				t.Errorf("error should reference parse failure, got: %v", err)
			}
		})
	}
}

func TestLoadGlobal_AcceptsAllDocumentedKeys(t *testing.T) {
	data := `agent: claude
acpx_path: /opt/bin/acpx
acp_registry_overrides:
  local-gemini: node /tmp/mock-acp.mjs
agent_path_override:
  claude: /usr/local/bin/claude
agent_args_override:
  codex:
    - -m
    - gpt-5.4
ci_timeout: "4h"
babysit_timeout: "90m"
log_level: info
auto_fix:
  rebase: 3
  lint: 3
  test: 3
  review: 0
  document: 3
  ci: 3
  babysit: 2
intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: [codex]
test:
  evidence:
    store_in_repo: true
    dir: .no-mistakes/evidence
`
	path := writeGlobalConfig(t, data)
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("expected documented keys to load cleanly, got: %v", err)
	}
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want claude", cfg.Agent)
	}
}

func TestLoadGlobal_RejectsAgentTypo(t *testing.T) {
	path := writeGlobalConfig(t, `agent: claud`)
	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatalf("expected error for typo'd agent, got nil")
	}
	for _, want := range []string{"agent", "claud", "global config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestLoadGlobal_RejectsAgentEmpty(t *testing.T) {
	path := writeGlobalConfig(t, `agent: ""`)
	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatalf("expected error for explicit empty agent, got nil")
	}
	for _, want := range []string{"agent", "empty", "global config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestLoadGlobal_RejectsAgentBareEmpty(t *testing.T) {
	path := writeGlobalConfig(t, "agent:\n")
	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatalf("expected error for bare empty agent, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty, got: %v", err)
	}
}

func TestLoadGlobal_RejectsACPEmptyTarget(t *testing.T) {
	path := writeGlobalConfig(t, `agent: "acp:"`)
	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatalf("expected error for acp: with empty target, got nil")
	}
	for _, want := range []string{"agent", "acp:", "global config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestLoadGlobal_RejectsACPWhitespaceTarget(t *testing.T) {
	path := writeGlobalConfig(t, `agent: "acp: foo"`)
	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatalf("expected error for acp: with whitespace target, got nil")
	}
	for _, want := range []string{"agent", "acp: foo", "global config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestLoadGlobal_AcceptsValidAgents(t *testing.T) {
	cases := map[string]types.AgentName{
		"auto":     types.AgentAuto,
		"claude":   types.AgentClaude,
		"codex":    types.AgentCodex,
		"rovodev":  types.AgentRovoDev,
		"opencode": types.AgentOpenCode,
		"pi":       types.AgentPi,
		"acp":      "acp:gemini",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeGlobalConfig(t, "agent: "+string(want))
			cfg, err := LoadGlobal(path)
			if err != nil {
				t.Fatalf("expected %q to load cleanly, got: %v", name, err)
			}
			if cfg.Agent != want {
				t.Errorf("agent = %q, want %q", cfg.Agent, want)
			}
		})
	}
}

func TestLoadGlobal_MissingAgentKeyUsesDefault(t *testing.T) {
	path := writeGlobalConfig(t, `log_level: debug`)
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("missing agent key should default to auto, got: %v", err)
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want auto (default)", cfg.Agent)
	}
}

func TestLoadRepo_RejectsUnknownTopLevelKey(t *testing.T) {
	dir := writeRepoConfig(t, `citimeout: "4h"`)
	_, err := LoadRepo(dir)
	if err == nil {
		t.Fatalf("expected error for unknown repo key, got nil")
	}
	if !strings.Contains(err.Error(), "parse repo config") {
		t.Errorf("error should reference parse failure, got: %v", err)
	}
}

func TestLoadRepo_RejectsUnknownNestedKey(t *testing.T) {
	cases := map[string]string{
		"commands_typo": `commands: { tst: "go test" }`,
		"auto_fix_typo": `auto_fix: { rebse: 3 }`,
		"intent_typo":   `intent: { thresh: 0.2 }`,
		"evidence_typo": `test: { evidence: { dirr: foo } }`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeRepoConfig(t, data)
			_, err := LoadRepo(dir)
			if err == nil {
				t.Fatalf("expected error for unknown nested repo key, got nil")
			}
			if !strings.Contains(err.Error(), "parse repo config") {
				t.Errorf("error should reference parse failure, got: %v", err)
			}
		})
	}
}

func TestLoadRepo_RejectsAgentTypo(t *testing.T) {
	dir := writeRepoConfig(t, `agent: claud`)
	_, err := LoadRepo(dir)
	if err == nil {
		t.Fatalf("expected error for typo'd repo agent, got nil")
	}
	for _, want := range []string{"agent", "claud", "repo config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestLoadRepo_RejectsACPEmptyTarget(t *testing.T) {
	dir := writeRepoConfig(t, `agent: "acp:"`)
	_, err := LoadRepo(dir)
	if err == nil {
		t.Fatalf("expected error for acp: with empty target in repo config, got nil")
	}
}

func TestLoadRepo_AcceptsEmptyAgentAsInherit(t *testing.T) {
	// Repo config: empty agent means "inherit from global". Both absent key
	// and explicit empty are tolerated.
	t.Run("absent", func(t *testing.T) {
		dir := writeRepoConfig(t, `commands: { test: "go test" }`)
		cfg, err := LoadRepo(dir)
		if err != nil {
			t.Fatalf("absent agent key should load cleanly, got: %v", err)
		}
		if cfg.Agent != "" {
			t.Errorf("agent = %q, want empty", cfg.Agent)
		}
	})
	t.Run("explicit_empty", func(t *testing.T) {
		dir := writeRepoConfig(t, `agent: ""`)
		cfg, err := LoadRepo(dir)
		if err != nil {
			t.Fatalf("explicit empty agent in repo config should be tolerated as inherit, got: %v", err)
		}
		if cfg.Agent != "" {
			t.Errorf("agent = %q, want empty", cfg.Agent)
		}
	})
}

func TestLoadRepo_AcceptsAllDocumentedKeys(t *testing.T) {
	data := `agent: codex
commands:
  lint: "golangci-lint run ./..."
  test: "go test -race ./..."
  format: "gofmt -w ."
ignore_patterns:
  - "*.generated.go"
auto_fix:
  rebase: 3
  review: 3
  test: 3
  document: 3
  lint: 5
  ci: 3
  babysit: 2
intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: [codex]
test:
  evidence:
    store_in_repo: true
    dir: .no-mistakes/evidence
`
	dir := writeRepoConfig(t, data)
	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("expected documented repo keys to load cleanly, got: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want codex", cfg.Agent)
	}
}

func TestValidateAgentName(t *testing.T) {
	cases := []struct {
		name    string
		agent   types.AgentName
		wantErr bool
	}{
		{"empty", "", false},
		{"auto", types.AgentAuto, false},
		{"claude", types.AgentClaude, false},
		{"codex", types.AgentCodex, false},
		{"rovodev", types.AgentRovoDev, false},
		{"opencode", types.AgentOpenCode, false},
		{"pi", types.AgentPi, false},
		{"acp_gemini", "acp:gemini", false},
		{"typo_claud", "claud", true},
		{"typo_codexx", "codexx", true},
		{"acp_empty", "acp:", true},
		{"acp_whitespace", "acp: foo", true},
		{"acp_tab", "acp:\tfoo", true},
		{"random", "some-other-thing", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentName(tc.agent)
			if tc.wantErr && err == nil {
				t.Errorf("validateAgentName(%q) = nil, want error", tc.agent)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateAgentName(%q) = %v, want nil", tc.agent, err)
			}
		})
	}
}
