package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestLoadGlobal_Defaults(t *testing.T) {
	// Non-existent file should return defaults
	cfg, err := LoadGlobal("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CITimeout != DefaultCITimeout {
		t.Errorf("ci_timeout = %v, want %v", cfg.CITimeout, DefaultCITimeout)
	}
	if cfg.StepQuietWarning != DefaultStepQuietWarning {
		t.Errorf("step_quiet_warning = %v, want %v", cfg.StepQuietWarning, DefaultStepQuietWarning)
	}
	if cfg.DaemonConnectTimeout != DefaultDaemonConnectTimeout {
		t.Errorf("daemon_connect_timeout = %v, want %v", cfg.DaemonConnectTimeout, DefaultDaemonConnectTimeout)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "info")
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
		"ci_timeout:",
		"step_quiet_warning:",
		"daemon_connect_timeout:",
		"log_level: info",
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
	if cfg.CITimeout != DefaultCITimeout {
		t.Errorf("ci_timeout = %v, want %v", cfg.CITimeout, DefaultCITimeout)
	}
	if cfg.StepQuietWarning != DefaultStepQuietWarning {
		t.Errorf("step_quiet_warning = %v, want %v", cfg.StepQuietWarning, DefaultStepQuietWarning)
	}
	if cfg.DaemonConnectTimeout != DefaultDaemonConnectTimeout {
		t.Errorf("daemon_connect_timeout = %v, want %v", cfg.DaemonConnectTimeout, DefaultDaemonConnectTimeout)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "info")
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

func TestEnsureDefaultGlobalConfig_DoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	custom := "log_level: debug\n"
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
	if err := os.WriteFile(path, []byte("log_level: debug\n"), 0o644); err != nil {
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
	if string(data) != "log_level: debug\n" {
		t.Errorf("config was overwritten despite stat permission error:\ngot:  %q\nwant: %q", string(data), "log_level: debug\n")
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
	data := `ci_timeout: "2h30m"
daemon_connect_timeout: "4s"
log_level: "debug"
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.CITimeout != 2*time.Hour+30*time.Minute {
		t.Errorf("ci_timeout = %v, want %v", cfg.CITimeout, 2*time.Hour+30*time.Minute)
	}
	if cfg.DaemonConnectTimeout != 4*time.Second {
		t.Errorf("daemon_connect_timeout = %v, want 4s", cfg.DaemonConnectTimeout)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "debug")
	}

}

func TestLoadGlobal_PartialOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Override only log level; the timeout should retain its default.
	data := `log_level: debug
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CITimeout != DefaultCITimeout {
		t.Errorf("ci_timeout = %v, want %v (should be default)", cfg.CITimeout, DefaultCITimeout)
	}
	if cfg.DaemonConnectTimeout != DefaultDaemonConnectTimeout {
		t.Errorf("daemon_connect_timeout = %v, want %v (should be default)", cfg.DaemonConnectTimeout, DefaultDaemonConnectTimeout)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "debug")
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

func TestDefaultConfigYAML_MatchesGoDefaults(t *testing.T) {
	var raw globalConfigRaw
	if err := yaml.Unmarshal([]byte(defaultConfigYAML), &raw); err != nil {
		t.Fatalf("defaultConfigYAML is not valid YAML: %v", err)
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
	if raw.LogLevel != "info" {
		t.Errorf("YAML log_level = %q, Go default = %q", raw.LogLevel, "info")
	}
	if raw.SessionReuse == nil || !*raw.SessionReuse {
		t.Errorf("YAML session_reuse = %v, Go default = true", raw.SessionReuse)
	}
}
