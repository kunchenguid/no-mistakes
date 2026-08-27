package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

func TestLoadGlobal_Defaults(t *testing.T) {
	// Non-existent file should return defaults
	cfg, err := LoadGlobal("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentAuto)
	}
	if cfg.CITimeout != DefaultCITimeout {
		t.Errorf("ci_timeout = %v, want %v", cfg.CITimeout, DefaultCITimeout)
	}
	if cfg.StepQuietWarning != DefaultStepQuietWarning {
		t.Errorf("step_quiet_warning = %v, want %v", cfg.StepQuietWarning, DefaultStepQuietWarning)
	}
	if cfg.ReviewAgentTimeout != DefaultReviewAgentTimeout {
		t.Errorf("review_agent_timeout = %v, want %v", cfg.ReviewAgentTimeout, DefaultReviewAgentTimeout)
	}
	if cfg.AgentTimeout != DefaultAgentTimeout {
		t.Errorf("agent_timeout = %v, want %v", cfg.AgentTimeout, DefaultAgentTimeout)
	}
	if cfg.TestAgentTimeout != DefaultTestAgentTimeout {
		t.Errorf("test_agent_timeout = %v, want %v", cfg.TestAgentTimeout, DefaultTestAgentTimeout)
	}
	if cfg.DaemonConnectTimeout != DefaultDaemonConnectTimeout {
		t.Errorf("daemon_connect_timeout = %v, want %v", cfg.DaemonConnectTimeout, DefaultDaemonConnectTimeout)
	}
	if cfg.BranchSyncRemoteTimeout != DefaultBranchSyncRemoteTimeout {
		t.Errorf("branch_sync_remote_timeout = %v, want %v", cfg.BranchSyncRemoteTimeout, DefaultBranchSyncRemoteTimeout)
	}
	if cfg.GateReconcileInterval != DefaultGateReconcileInterval {
		t.Errorf("gate_reconcile_interval = %v, want %v", cfg.GateReconcileInterval, DefaultGateReconcileInterval)
	}
	if cfg.GateReconcileTimeout != DefaultGateReconcileTimeout {
		t.Errorf("gate_reconcile_timeout = %v, want %v", cfg.GateReconcileTimeout, DefaultGateReconcileTimeout)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "info")
	}
	if len(cfg.AgentPathOverride) != 0 {
		t.Errorf("agent_path_override = %v, want empty", cfg.AgentPathOverride)
	}
	if cfg.ForgejoAXIPath != "forgejo-axi" {
		t.Errorf("forgejo_axi_path = %q, want forgejo-axi", cfg.ForgejoAXIPath)
	}
}

func TestEnsureDefaultGlobalConfig_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	EnsureDefaultGlobalConfig(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file not created: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"agent: auto",
		"ci_timeout:",
		"step_quiet_warning:",
		"review_agent_timeout:",
		"agent_timeout:",
		"test_agent_timeout:",
		"daemon_connect_timeout:",
		"branch_sync_remote_timeout:",
		"log_level: info",
		"# agent_path_override:",
		"# commit:",
		`#   fix_message: "no-mistakes({{.Step}}): {{.Summary}}"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("default config missing %q", want)
		}
	}
}

func TestEnsureDefaultGlobalConfig_CreatedConfigIsLoadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	EnsureDefaultGlobalConfig(path)

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error on reload: %v", err)
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentAuto)
	}
	if cfg.CITimeout != DefaultCITimeout {
		t.Errorf("ci_timeout = %v, want %v", cfg.CITimeout, DefaultCITimeout)
	}
	if cfg.StepQuietWarning != DefaultStepQuietWarning {
		t.Errorf("step_quiet_warning = %v, want %v", cfg.StepQuietWarning, DefaultStepQuietWarning)
	}
	if cfg.ReviewAgentTimeout != DefaultReviewAgentTimeout {
		t.Errorf("review_agent_timeout = %v, want %v", cfg.ReviewAgentTimeout, DefaultReviewAgentTimeout)
	}
	if cfg.AgentTimeout != DefaultAgentTimeout {
		t.Errorf("agent_timeout = %v, want %v", cfg.AgentTimeout, DefaultAgentTimeout)
	}
	if cfg.TestAgentTimeout != DefaultTestAgentTimeout {
		t.Errorf("test_agent_timeout = %v, want %v", cfg.TestAgentTimeout, DefaultTestAgentTimeout)
	}
	if cfg.DaemonConnectTimeout != DefaultDaemonConnectTimeout {
		t.Errorf("daemon_connect_timeout = %v, want %v", cfg.DaemonConnectTimeout, DefaultDaemonConnectTimeout)
	}
	if cfg.BranchSyncRemoteTimeout != DefaultBranchSyncRemoteTimeout {
		t.Errorf("branch_sync_remote_timeout = %v, want %v", cfg.BranchSyncRemoteTimeout, DefaultBranchSyncRemoteTimeout)
	}
	if cfg.GateReconcileInterval != DefaultGateReconcileInterval {
		t.Errorf("gate_reconcile_interval = %v, want %v", cfg.GateReconcileInterval, DefaultGateReconcileInterval)
	}
	if cfg.GateReconcileTimeout != DefaultGateReconcileTimeout {
		t.Errorf("gate_reconcile_timeout = %v, want %v", cfg.GateReconcileTimeout, DefaultGateReconcileTimeout)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.ForgejoAXIPath != "forgejo-axi" {
		t.Errorf("forgejo_axi_path = %q, want forgejo-axi", cfg.ForgejoAXIPath)
	}
}

func TestLoadGlobal_ForgejoAXIPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("forgejo_axi_path: /opt/tools/forgejo-axi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.ForgejoAXIPath != "/opt/tools/forgejo-axi" {
		t.Fatalf("forgejo_axi_path = %q, want configured executable", cfg.ForgejoAXIPath)
	}
	merged := Merge(cfg, &RepoConfig{})
	if merged.ForgejoAXIPath != cfg.ForgejoAXIPath {
		t.Fatalf("merged forgejo_axi_path = %q, want %q", merged.ForgejoAXIPath, cfg.ForgejoAXIPath)
	}
}

func TestLoadGlobal_StepQuietWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("step_quiet_warning: 90s\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.StepQuietWarning != 90*time.Second {
		t.Fatalf("step_quiet_warning = %v, want 90s", cfg.StepQuietWarning)
	}
}

func TestLoadGlobal_AgentTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent_timeout: 90s\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.AgentTimeout != 90*time.Second {
		t.Fatalf("agent_timeout = %v, want 90s", cfg.AgentTimeout)
	}
}

func TestLoadGlobal_ReviewAgentTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("review_agent_timeout: 90s\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.ReviewAgentTimeout != 90*time.Second {
		t.Fatalf("review_agent_timeout = %v, want 90s", cfg.ReviewAgentTimeout)
	}
}

func TestLoadGlobal_TestAgentTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("test_agent_timeout: 90s\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.TestAgentTimeout != 90*time.Second {
		t.Fatalf("test_agent_timeout = %v, want 90s", cfg.TestAgentTimeout)
	}
}

func TestLoadGlobal_GateReconcileTimings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("gate_reconcile_interval: 45s\ngate_reconcile_timeout: 90s\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.GateReconcileInterval != 45*time.Second {
		t.Fatalf("gate_reconcile_interval = %v, want 45s", cfg.GateReconcileInterval)
	}
	if cfg.GateReconcileTimeout != 90*time.Second {
		t.Fatalf("gate_reconcile_timeout = %v, want 90s", cfg.GateReconcileTimeout)
	}
}

// TestLoadGlobal_GateReconcileTimings_OperatorSlowAuthBudget is the documented
// operator path: raise interval/timeout in global config.yaml so slow gh auth
// probes fit the parked-gate reconcile budget (defaults remain 2m / 30s).
func TestLoadGlobal_GateReconcileTimings_OperatorSlowAuthBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "gate_reconcile_interval: \"5m\"\ngate_reconcile_timeout: \"2m\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.GateReconcileInterval != 5*time.Minute {
		t.Fatalf("gate_reconcile_interval = %v, want 5m", cfg.GateReconcileInterval)
	}
	if cfg.GateReconcileTimeout != 2*time.Minute {
		t.Fatalf("gate_reconcile_timeout = %v, want 2m", cfg.GateReconcileTimeout)
	}
	merged := Merge(cfg, &RepoConfig{})
	if merged.GateReconcileInterval != 5*time.Minute || merged.GateReconcileTimeout != 2*time.Minute {
		t.Fatalf("Merge did not preserve global timings: interval=%v timeout=%v",
			merged.GateReconcileInterval, merged.GateReconcileTimeout)
	}
	t.Logf("operator config.yaml loaded: gate_reconcile_interval=%v gate_reconcile_timeout=%v",
		merged.GateReconcileInterval, merged.GateReconcileTimeout)
}

func TestLoadGlobal_InvalidGateReconcileTimings(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{
		`gate_reconcile_timeout: "not-a-duration"`,
		`gate_reconcile_timeout: "0s"`,
		`gate_reconcile_timeout: "-1s"`,
		`gate_reconcile_interval: "0s"`,
	} {
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadGlobal(path); err == nil {
			t.Fatalf("LoadGlobal(%q) error = nil, want error", body)
		}
	}
}

func TestLoadGlobal_InvalidAgentTimeout(t *testing.T) {
	cases := []string{
		`agent_timeout: "not-a-duration"`,
		`agent_timeout: "0s"`,
		`agent_timeout: "-1s"`,
	}
	for _, data := range cases {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadGlobal(path)
			if err == nil {
				t.Fatal("expected error for invalid agent_timeout")
			}
		})
	}
}

func TestLoadGlobal_InvalidTestAgentTimeout(t *testing.T) {
	cases := []string{
		`test_agent_timeout: "not-a-duration"`,
		`test_agent_timeout: "0s"`,
		`test_agent_timeout: "-1s"`,
	}
	for _, data := range cases {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadGlobal(path)
			if err == nil {
				t.Fatal("expected error for invalid test_agent_timeout")
			}
		})
	}
}

func TestEnsureDefaultGlobalConfig_DoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	custom := "agent: codex\nlog_level: debug\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	EnsureDefaultGlobalConfig(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}
	if string(data) != custom {
		t.Errorf("config was overwritten:\ngot:  %q\nwant: %q", string(data), custom)
	}
}

func TestEnsureDefaultGlobalConfig_SkipsOnStatPermissionError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent: codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skip("cannot restrict directory permissions")
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	EnsureDefaultGlobalConfig(path)

	os.Chmod(dir, 0o755)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}
	if string(data) != "agent: codex\n" {
		t.Errorf("config was overwritten despite stat permission error:\ngot:  %q\nwant: %q", string(data), "agent: codex\n")
	}
}

func TestEnsureDefaultGlobalConfig_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "config.yaml")

	EnsureDefaultGlobalConfig(path)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not created in nested dir: %v", err)
	}
}

func TestLoadGlobal_DoesNotCreateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	_, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(path); err == nil {
		t.Error("LoadGlobal should not create config file")
	}
}

func TestLoadGlobal_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `agent: codex
agent_path_override:
  claude: /usr/local/bin/claude
  codex: /opt/codex
ci_timeout: "2h30m"
daemon_connect_timeout: "4s"
branch_sync_remote_timeout: "90s"
log_level: "debug"
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
	if cfg.CITimeout != 2*time.Hour+30*time.Minute {
		t.Errorf("ci_timeout = %v, want %v", cfg.CITimeout, 2*time.Hour+30*time.Minute)
	}
	if cfg.DaemonConnectTimeout != 4*time.Second {
		t.Errorf("daemon_connect_timeout = %v, want 4s", cfg.DaemonConnectTimeout)
	}
	if cfg.BranchSyncRemoteTimeout != 90*time.Second {
		t.Errorf("branch_sync_remote_timeout = %v, want 90s", cfg.BranchSyncRemoteTimeout)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.AgentPathOverride["claude"] != "/usr/local/bin/claude" {
		t.Errorf("claude path = %q, want %q", cfg.AgentPathOverride["claude"], "/usr/local/bin/claude")
	}
	if cfg.AgentPathOverride["codex"] != "/opt/codex" {
		t.Errorf("codex path = %q, want %q", cfg.AgentPathOverride["codex"], "/opt/codex")
	}
}

func TestLoadGlobal_AgentAcceptsList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `agent: [codex, claude]
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
	want := []types.AgentName{types.AgentCodex, types.AgentClaude}
	if len(cfg.Agents) != len(want) {
		t.Fatalf("agents = %v, want %v", cfg.Agents, want)
	}
	for i := range want {
		if cfg.Agents[i] != want[i] {
			t.Fatalf("agents = %v, want %v", cfg.Agents, want)
		}
	}
}

func TestLoadGlobal_AgentStringPreservesSingleAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `agent: codex
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentCodex {
		t.Fatalf("agents = %v, want [codex]", cfg.Agents)
	}
}

func TestLoadGlobal_PartialOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Only override agent, rest should be defaults
	data := `agent: opencode
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentOpenCode {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentOpenCode)
	}
	if cfg.CITimeout != DefaultCITimeout {
		t.Errorf("ci_timeout = %v, want %v (should be default)", cfg.CITimeout, DefaultCITimeout)
	}
	if cfg.DaemonConnectTimeout != DefaultDaemonConnectTimeout {
		t.Errorf("daemon_connect_timeout = %v, want %v (should be default)", cfg.DaemonConnectTimeout, DefaultDaemonConnectTimeout)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, want %q (should be default)", cfg.LogLevel, "info")
	}
}

func TestLoadGlobal_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("{{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadGlobal_InvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`ci_timeout: "not-a-duration"`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestLoadGlobal_InvalidDaemonConnectTimeout(t *testing.T) {
	cases := []string{
		`daemon_connect_timeout: "not-a-duration"`,
		`daemon_connect_timeout: "0s"`,
		`daemon_connect_timeout: "-1s"`,
	}
	for _, data := range cases {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadGlobal(path)
			if err == nil {
				t.Fatal("expected error for invalid daemon_connect_timeout")
			}
		})
	}
}

func TestLoadGlobal_InvalidBranchSyncRemoteTimeout(t *testing.T) {
	cases := []string{
		`branch_sync_remote_timeout: "not-a-duration"`,
		`branch_sync_remote_timeout: "0s"`,
		`branch_sync_remote_timeout: "-1s"`,
	}
	for _, data := range cases {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadGlobal(path)
			if err == nil {
				t.Fatal("expected error for invalid branch_sync_remote_timeout")
			}
		})
	}
}

func TestLoadGlobal_CITimeoutUnlimited(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"keyword", `ci_timeout: "unlimited"`},
		{"keyword_none", `ci_timeout: "none"`},
		{"keyword_mixed_case", `ci_timeout: "Unlimited"`},
		{"zero", `ci_timeout: "0"`},
		{"zero_seconds", `ci_timeout: "0s"`},
		{"negative", `ci_timeout: "-5m"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(tc.value), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadGlobal(path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.CITimeout != CITimeoutUnlimited {
				t.Fatalf("ci_timeout = %v, want CITimeoutUnlimited (%v)", cfg.CITimeout, CITimeoutUnlimited)
			}
		})
	}
}

func TestLoadGlobal_LegacyBabysitTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`babysit_timeout: "90m"`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CITimeout != 90*time.Minute {
		t.Fatalf("ci_timeout = %v, want %v", cfg.CITimeout, 90*time.Minute)
	}
}

func TestLoadGlobal_LegacyAutoFixBabysit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("auto_fix:\n  babysit: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoFix.CI == nil {
		t.Fatal("ci auto-fix override was not loaded")
	}
	if *cfg.AutoFix.CI != 0 {
		t.Fatalf("ci auto-fix = %d, want 0", *cfg.AutoFix.CI)
	}
}

func TestDefaultConfigYAML_MatchesGoDefaults(t *testing.T) {
	var raw globalConfigRaw
	if err := yaml.Unmarshal([]byte(defaultConfigYAML), &raw); err != nil {
		t.Fatalf("defaultConfigYAML is not valid YAML: %v", err)
	}

	if len(raw.Agent) != 1 || raw.Agent[0] != types.AgentAuto {
		t.Errorf("YAML agent = %q, Go default = %q", raw.Agent, types.AgentAuto)
	}
	d, err := time.ParseDuration(raw.CITimeout)
	if err != nil {
		t.Fatalf("YAML ci_timeout %q is not a valid duration: %v", raw.CITimeout, err)
	}
	if d != DefaultCITimeout {
		t.Errorf("YAML ci_timeout = %v, Go default = %v", d, DefaultCITimeout)
	}
	d, err = time.ParseDuration(raw.DaemonConnectTimeout)
	if err != nil {
		t.Fatalf("YAML daemon_connect_timeout %q is not a valid duration: %v", raw.DaemonConnectTimeout, err)
	}
	if d != DefaultDaemonConnectTimeout {
		t.Errorf("YAML daemon_connect_timeout = %v, Go default = %v", d, DefaultDaemonConnectTimeout)
	}
	d, err = time.ParseDuration(raw.BranchSyncRemoteTimeout)
	if err != nil {
		t.Fatalf("YAML branch_sync_remote_timeout %q is not a valid duration: %v", raw.BranchSyncRemoteTimeout, err)
	}
	if d != DefaultBranchSyncRemoteTimeout {
		t.Errorf("YAML branch_sync_remote_timeout = %v, Go default = %v", d, DefaultBranchSyncRemoteTimeout)
	}
	d, err = time.ParseDuration(raw.GateReconcileInterval)
	if err != nil {
		t.Fatalf("YAML gate_reconcile_interval %q is not a valid duration: %v", raw.GateReconcileInterval, err)
	}
	if d != DefaultGateReconcileInterval {
		t.Errorf("YAML gate_reconcile_interval = %v, Go default = %v", d, DefaultGateReconcileInterval)
	}
	d, err = time.ParseDuration(raw.GateReconcileTimeout)
	if err != nil {
		t.Fatalf("YAML gate_reconcile_timeout %q is not a valid duration: %v", raw.GateReconcileTimeout, err)
	}
	if d != DefaultGateReconcileTimeout {
		t.Errorf("YAML gate_reconcile_timeout = %v, Go default = %v", d, DefaultGateReconcileTimeout)
	}
	d, err = time.ParseDuration(raw.ReviewAgentTimeout)
	if err != nil {
		t.Fatalf("YAML review_agent_timeout %q is not a valid duration: %v", raw.ReviewAgentTimeout, err)
	}
	if d != DefaultReviewAgentTimeout {
		t.Errorf("YAML review_agent_timeout = %v, Go default = %v", d, DefaultReviewAgentTimeout)
	}
	d, err = time.ParseDuration(raw.AgentTimeout)
	if err != nil {
		t.Fatalf("YAML agent_timeout %q is not a valid duration: %v", raw.AgentTimeout, err)
	}
	if d != DefaultAgentTimeout {
		t.Errorf("YAML agent_timeout = %v, Go default = %v", d, DefaultAgentTimeout)
	}
	d, err = time.ParseDuration(raw.TestAgentTimeout)
	if err != nil {
		t.Fatalf("YAML test_agent_timeout %q is not a valid duration: %v", raw.TestAgentTimeout, err)
	}
	if d != DefaultTestAgentTimeout {
		t.Errorf("YAML test_agent_timeout = %v, Go default = %v", d, DefaultTestAgentTimeout)
	}
	if raw.LogLevel != "info" {
		t.Errorf("YAML log_level = %q, Go default = %q", raw.LogLevel, "info")
	}
	if raw.SessionReuse == nil || !*raw.SessionReuse {
		t.Errorf("YAML session_reuse = %v, Go default = true", raw.SessionReuse)
	}
	defaults := autoFixDefaults()
	if raw.AutoFix.Lint == nil || *raw.AutoFix.Lint != defaults.Lint {
		t.Errorf("YAML auto_fix.lint = %v, Go default = %d", raw.AutoFix.Lint, defaults.Lint)
	}
	if raw.AutoFix.Test == nil || *raw.AutoFix.Test != defaults.Test {
		t.Errorf("YAML auto_fix.test = %v, Go default = %d", raw.AutoFix.Test, defaults.Test)
	}
	if raw.AutoFix.Review == nil || *raw.AutoFix.Review != defaults.Review {
		t.Errorf("YAML auto_fix.review = %v, Go default = %d", raw.AutoFix.Review, defaults.Review)
	}
	if raw.AutoFix.Document == nil || *raw.AutoFix.Document != defaults.Document {
		t.Errorf("YAML auto_fix.document = %v, Go default = %d", raw.AutoFix.Document, defaults.Document)
	}
	if raw.AutoFix.CI == nil || *raw.AutoFix.CI != defaults.CI {
		t.Errorf("YAML auto_fix.ci = %v, Go default = %d", raw.AutoFix.CI, defaults.CI)
	}
	if raw.AutoFix.Rebase == nil || *raw.AutoFix.Rebase != defaults.Rebase {
		t.Errorf("YAML auto_fix.rebase = %v, Go default = %d", raw.AutoFix.Rebase, defaults.Rebase)
	}
	if raw.CI.RerunTransient == nil || *raw.CI.RerunTransient != ciDefaults().RerunTransient {
		t.Errorf("YAML ci.rerun_transient = %v, Go default = %d", raw.CI.RerunTransient, ciDefaults().RerunTransient)
	}
}

func TestLoadGlobal_AutoFixDefaults(t *testing.T) {
	cfg, err := LoadGlobal("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// AutoFix should be nil (unset) in GlobalConfig
	if cfg.AutoFix.Lint != nil || cfg.AutoFix.Test != nil || cfg.AutoFix.Review != nil ||
		cfg.AutoFix.Document != nil || cfg.AutoFix.CI != nil || cfg.AutoFix.Rebase != nil {
		t.Errorf("expected all AutoFix fields to be nil for defaults, got %+v", cfg.AutoFix)
	}
}

func TestLoadGlobal_AutoFixFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `auto_fix:
  lint: 5
  test: 0
  review: 2
  ci: 1
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoFix.Lint == nil || *cfg.AutoFix.Lint != 5 {
		t.Errorf("lint = %v, want 5", cfg.AutoFix.Lint)
	}
	if cfg.AutoFix.Test == nil || *cfg.AutoFix.Test != 0 {
		t.Errorf("test = %v, want 0", cfg.AutoFix.Test)
	}
	if cfg.AutoFix.Review == nil || *cfg.AutoFix.Review != 2 {
		t.Errorf("review = %v, want 2", cfg.AutoFix.Review)
	}
	if cfg.AutoFix.CI == nil || *cfg.AutoFix.CI != 1 {
		t.Errorf("ci =%v, want 1", cfg.AutoFix.CI)
	}
}

func TestLoadGlobal_AutoFixPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `auto_fix:
  lint: 1
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoFix.Lint == nil || *cfg.AutoFix.Lint != 1 {
		t.Errorf("lint = %v, want 1", cfg.AutoFix.Lint)
	}
	// Unset fields should remain nil
	if cfg.AutoFix.Test != nil {
		t.Errorf("test = %v, want nil", cfg.AutoFix.Test)
	}
}
