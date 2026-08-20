package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEvalDefaultsCollectWithoutSetup pins the decision that replaced the
// environment variable: a machine with no eval configuration at all still
// records replay provenance and still collects cases. Provenance cannot be
// added to a review round after the fact, so an off-by-default setting means
// every run before someone discovers the setting is permanently unusable.
func TestEvalDefaultsCollectWithoutSetup(t *testing.T) {
	cfg, err := LoadGlobal(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Eval.CaptureProvenance || !cfg.Eval.AutoCapture || cfg.Eval.MaxCases != DefaultEvalMaxCases || cfg.Eval.DiversifiedSize != DefaultEvalDiversifiedSize {
		t.Fatalf("eval defaults = %#v, want provenance and auto-capture on with the default caps", cfg.Eval)
	}
	merged := Merge(cfg, &RepoConfig{})
	if merged.Eval != cfg.Eval {
		t.Fatalf("merged eval = %#v, want the global values %#v", merged.Eval, cfg.Eval)
	}
}

func TestEvalSettingsAreConfigurable(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("eval:\n  capture_provenance: false\n  auto_capture: false\n  max_cases: 25\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Eval.CaptureProvenance || cfg.Eval.AutoCapture || cfg.Eval.MaxCases != 25 {
		t.Fatalf("eval config = %#v, want both halves off and a cap of 25", cfg.Eval)
	}
}

// TestEvalMaxCasesZeroKeepsEveryCase pins that an explicit zero survives the
// "not set" pointer check instead of falling back to the default cap.
func TestEvalMaxCasesZeroKeepsEveryCase(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("eval:\n  max_cases: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Eval.MaxCases != 0 {
		t.Fatalf("eval.max_cases = %d, want 0 (keep every case)", cfg.Eval.MaxCases)
	}
}

func TestLoadGlobalRejectsNegativeEvalMaxCases(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("eval:\n  max_cases: -1\n"))
	if err == nil || !strings.Contains(err.Error(), "eval.max_cases") {
		t.Fatalf("error = %v, want a rejected negative eval.max_cases", err)
	}
}

func TestLoadGlobalRejectsNegativeEvalDiversifiedSize(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("eval:\n  diversified_size: -1\n"))
	if err == nil || !strings.Contains(err.Error(), "eval.diversified_size") {
		t.Fatalf("error = %v, want a rejected negative eval.diversified_size", err)
	}
}

func TestEvalDiversifiedSizeZeroMeansNoCap(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("eval:\n  diversified_size: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Eval.DiversifiedSize != 0 {
		t.Fatalf("eval.diversified_size = %d, want 0 (one gold case per stratum)", cfg.Eval.DiversifiedSize)
	}
}

// TestRepoConfigCannotChangeEvalCollection pins eval as an operator-only,
// machine-level setting. It governs this machine's local disk and what its
// daemon records, so a pushed branch must not be able to switch collection on,
// off, or resize it for the person running the pipeline.
func TestRepoConfigCannotChangeEvalCollection(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("eval:\n  auto_capture: true\n  max_cases: 7\n"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte("eval:\n  auto_capture: false\n  capture_provenance: false\n  max_cases: 9999\n  diversified_size: 1\n"))
	if err != nil {
		t.Fatalf("repo config with an eval block must load, ignoring the key: %v", err)
	}
	merged := Merge(global, repo)
	if !merged.Eval.AutoCapture || !merged.Eval.CaptureProvenance || merged.Eval.MaxCases != 7 || merged.Eval.DiversifiedSize != DefaultEvalDiversifiedSize {
		t.Fatalf("merged eval = %#v, want the operator's global values untouched by the repository", merged.Eval)
	}
}

// TestDefaultConfigYAMLDocumentsEvalCollection keeps the shipped template
// honest about a default that writes to disk on every run: someone reading
// their own config.yaml has to be able to see it and turn it off.
func TestDefaultConfigYAMLDocumentsEvalCollection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	EnsureDefaultGlobalConfig(path)
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobalFromBytes(written)
	if err != nil {
		t.Fatalf("shipped default config does not load: %v", err)
	}
	if cfg.Eval != evalDefaults() {
		t.Fatalf("shipped default config eval = %#v, want Go defaults %#v", cfg.Eval, evalDefaults())
	}
}
