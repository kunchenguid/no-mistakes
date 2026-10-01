package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_CommandConfigurationRecordedBeforeChecks(t *testing.T) {
	database, p, run, repo := setupTest(t)
	global, err := config.LoadGlobalFromBytes([]byte(`repository_overrides:
  https://github.com/test/repo:
    commands:
      test:
        additional: [extra-tests]
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.MergeForRemote(global, &config.RepoConfig{Commands: config.Commands{Test: "team-tests"}}, repo.UpstreamURL)
	cfg.TrustedConfigSHA = "trusted-commit"
	cfg.CommandOverrides["lint"] = config.CommandOverride{Nice: 10}
	path := filepath.Join(p.RunLogDir(run.ID), commandConfigurationFile)
	step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("effective configuration missing before checks: %v", err)
		}
		var evidence struct {
			EffectiveConfig config.Config `json:"effective_config"`
		}
		if err := json.Unmarshal(payload, &evidence); err != nil {
			t.Fatal(err)
		}
		if evidence.EffectiveConfig.Commands != cfg.Commands || evidence.EffectiveConfig.TrustedConfigSHA != cfg.TrustedConfigSHA || !reflect.DeepEqual(evidence.EffectiveConfig.CommandOverrides, cfg.CommandOverrides) {
			t.Fatalf("recorded configuration differs from effective configuration: %+v", evidence.EffectiveConfig)
		}
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestExecutor_CommandConfigurationRestrictsExistingFileBeforeChecks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not implement POSIX permission bits")
	}
	database, p, run, repo := setupTest(t)
	path := filepath.Join(p.RunLogDir(run.ID), commandConfigurationFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := []byte("{\"event\":\"previous\"}\n")
	if err := os.WriteFile(path, previous, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CommandOverrides: map[string]config.CommandOverride{"test": {Additional: []string{"extra-tests"}}}}
	step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("snapshot permissions before checks = %04o, want 0600", got)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(data), string(previous)) {
			t.Fatalf("previous snapshot was overwritten: %q", data)
		}
		var snapshot struct {
			InheritedEnv map[string]string `json:"inherited_environment"`
		}
		if err := json.Unmarshal(data[len(previous):], &snapshot); err != nil {
			t.Fatal(err)
		}
		if got, ok := snapshot.InheritedEnv["PATH"]; !ok || got != os.Getenv("PATH") {
			t.Fatal("inherited PATH missing from the appended snapshot")
		}
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestExecutor_NoOverrideCreatesNoCommandConfigurationEvidence(t *testing.T) {
	database, p, run, repo := setupTest(t)
	executor := NewExecutor(database, p, &config.Config{}, nil, []Step{newPassStep(types.StepTest)}, nil)
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.RunLogDir(run.ID), commandConfigurationFile)); !os.IsNotExist(err) {
		t.Fatalf("default behavior created command configuration evidence: %v", err)
	}
}

func TestExecutor_CommandConfigurationWriteFailureStopsBeforeChecks(t *testing.T) {
	database, p, run, repo := setupTest(t)
	path := filepath.Join(p.RunLogDir(run.ID), commandConfigurationFile)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	step := newPassStep(types.StepTest)
	cfg := &config.Config{CommandOverrides: map[string]config.CommandOverride{"test": {Additional: []string{"extra-tests"}}}}
	executor := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("ran without recording configuration evidence")
	}
	if step.callCount() != 0 {
		t.Fatal("checks ran despite an evidence write failure")
	}
}

func TestCommandConfiguration_AppendsRecoveryAndOverrideRemoval(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Commands: config.Commands{Test: "team-tests"}, CommandOverrides: map[string]config.CommandOverride{"test": {Additional: []string{"extra-tests"}}}}
	if err := recordCommandConfiguration(dir, "start", cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, commandConfigurationFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.CommandOverrides = nil
	if err := recordCommandConfiguration(dir, "resume", cfg); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("resumed snapshot is not private: %v, %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("configuration history = %q", data)
	}
	for i, line := range lines {
		var snapshot struct {
			Event           string        `json:"event"`
			EffectiveConfig config.Config `json:"effective_config"`
		}
		if err := json.Unmarshal([]byte(line), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.EffectiveConfig.Commands.Test != "team-tests" || (i == 0 && len(snapshot.EffectiveConfig.CommandOverrides["test"].Additional) != 1) || (i == 1 && (snapshot.Event != "resume" || len(snapshot.EffectiveConfig.CommandOverrides) != 0)) {
			t.Fatalf("snapshot %d = %+v", i, snapshot)
		}
	}
}

func TestCommandConfiguration_RecordsInheritedCommandEnvironment(t *testing.T) {
	t.Setenv("GOMAXPROCS", "3")
	t.Setenv("CARGO_BUILD_JOBS", "")
	os.Unsetenv("CARGO_BUILD_JOBS")
	dir := t.TempDir()
	cfg := &config.Config{CommandOverrides: map[string]config.CommandOverride{"test": {Additional: []string{"extra-tests"}}}}
	if err := recordCommandConfiguration(dir, "start", cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, commandConfigurationFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		InheritedEnv map[string]*string `json:"inherited_environment"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if got := snapshot.InheritedEnv["PATH"]; got == nil || *got != os.Getenv("PATH") {
		t.Fatalf("PATH not recorded as inherited: %v", snapshot.InheritedEnv)
	}
	if got := snapshot.InheritedEnv["GOMAXPROCS"]; got == nil || *got != "3" {
		t.Fatalf("GOMAXPROCS not recorded as inherited: %v", snapshot.InheritedEnv)
	}
	if _, ok := snapshot.InheritedEnv["CARGO_BUILD_JOBS"]; ok {
		t.Fatalf("unset variable recorded: %v", snapshot.InheritedEnv)
	}
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("snapshot is not private: %v, %v", info, err)
	}
}
