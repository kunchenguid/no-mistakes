package config

import (
	"strings"
	"testing"
	"time"
)

// TestEvidenceLocalStorageIsGlobalOnly is the trust-boundary regression.
//
// local_root names a filesystem path the daemon writes to, and retention and
// max_runs budget a resource every repository on the machine shares. Neither is
// per-repository policy, so neither may be settable from a repository config -
// including the trusted default-branch copy, which is where test.evidence.branch
// legitimately comes from. Merge must resolve all three from GlobalConfig alone.
func TestEvidenceLocalStorageIsGlobalOnly(t *testing.T) {
	globalRoot := t.TempDir()
	repoRoot := t.TempDir()

	global, err := LoadGlobalFromBytes([]byte("test:\n  evidence:\n    local_root: " + globalRoot + "\n    retention: 100h\n    max_runs: 7\n"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte("test:\n  evidence:\n    local_root: " + repoRoot + "\n    retention: 1h\n    max_runs: 999\n"))
	if err != nil {
		t.Fatal(err)
	}

	// A repository config reaches Merge only through EffectiveRepoConfig, so
	// run it through the trusted path too: even a maintainer's own default
	// branch must not move this machine's evidence directory.
	effective := EffectiveRepoConfig(repo, repo, true)
	merged := Merge(global, effective)

	if merged.Test.Evidence.LocalRoot != globalRoot {
		t.Errorf("local_root = %q, want the global value %q", merged.Test.Evidence.LocalRoot, globalRoot)
	}
	if merged.Test.Evidence.Retention != 100*time.Hour {
		t.Errorf("retention = %v, want the global value 100h", merged.Test.Evidence.Retention)
	}
	if merged.Test.Evidence.MaxRuns != 7 {
		t.Errorf("max_runs = %d, want the global value 7", merged.Test.Evidence.MaxRuns)
	}
}

// TestEvidenceLocalStorageFallsBackToDefaultsWhenOnlyTheRepoSetsIt is the same
// boundary from the other side: with nothing configured globally, a repository
// value must not fill the gap - the built-in defaults do.
func TestEvidenceLocalStorageFallsBackToDefaultsWhenOnlyTheRepoSetsIt(t *testing.T) {
	repoRoot := t.TempDir()
	repo, err := LoadRepoFromBytes([]byte("test:\n  evidence:\n    local_root: " + repoRoot + "\n    retention: 1h\n    max_runs: 999\n"))
	if err != nil {
		t.Fatal(err)
	}

	merged := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(repo, repo, true))

	if merged.Test.Evidence.LocalRoot != "" {
		t.Errorf("local_root = %q, want empty (the app-root default)", merged.Test.Evidence.LocalRoot)
	}
	if merged.Test.Evidence.Retention != DefaultEvidenceRetention {
		t.Errorf("retention = %v, want %v", merged.Test.Evidence.Retention, DefaultEvidenceRetention)
	}
	if merged.Test.Evidence.MaxRuns != DefaultEvidenceMaxRuns {
		t.Errorf("max_runs = %d, want %d", merged.Test.Evidence.MaxRuns, DefaultEvidenceMaxRuns)
	}
}

// TestEvidenceStorageDefaultsAreBounded states the shipped policy: evidence is
// reaped by no-mistakes on both axes out of the box, with no configuration.
// The whole point of the relocation is that cleanup stops depending on an OS
// temp-directory timer, so neither default may be "keep forever".
func TestEvidenceStorageDefaultsAreBounded(t *testing.T) {
	merged := Merge(DefaultGlobalConfig(), &RepoConfig{})

	if merged.Test.Evidence.Retention <= 0 {
		t.Errorf("default retention = %v, want a positive bound", merged.Test.Evidence.Retention)
	}
	if merged.Test.Evidence.MaxRuns <= 0 {
		t.Errorf("default max_runs = %d, want a positive bound", merged.Test.Evidence.MaxRuns)
	}
	if merged.Test.Evidence.LocalRoot != "" {
		t.Errorf("default local_root = %q, want empty so paths.EvidenceDir() decides", merged.Test.Evidence.LocalRoot)
	}
}

// TestEvidenceStorageOverridesParse covers the documented values an operator
// can write, including the keyword and zero forms that disable a bound.
func TestEvidenceStorageOverridesParse(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name          string
		yaml          string
		wantRetention time.Duration
		wantMaxRuns   int
	}{
		{"documented example", "test:\n  evidence:\n    local_root: " + root + "\n    retention: 720h\n    max_runs: 50\n", 720 * time.Hour, 50},
		{"unlimited keyword", "test:\n  evidence:\n    retention: unlimited\n", 0, DefaultEvidenceMaxRuns},
		{"never keyword", "test:\n  evidence:\n    retention: never\n", 0, DefaultEvidenceMaxRuns},
		{"zero max_runs", "test:\n  evidence:\n    max_runs: 0\n", DefaultEvidenceRetention, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			merged := Merge(global, &RepoConfig{})
			if merged.Test.Evidence.Retention != tc.wantRetention {
				t.Errorf("retention = %v, want %v", merged.Test.Evidence.Retention, tc.wantRetention)
			}
			if merged.Test.Evidence.MaxRuns != tc.wantMaxRuns {
				t.Errorf("max_runs = %d, want %d", merged.Test.Evidence.MaxRuns, tc.wantMaxRuns)
			}
		})
	}
}

// TestEvidenceStorageFailsClosedOnUnusableValues surfaces a typo in the config
// where the operator can fix it, instead of at run time when evidence would
// already have gone somewhere unexpected.
func TestEvidenceStorageFailsClosedOnUnusableValues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		yaml     string
		wantHint string
	}{
		{"relative local_root", "test:\n  evidence:\n    local_root: evidence\n", "absolute"},
		{"dot-relative local_root", "test:\n  evidence:\n    local_root: ./evidence\n", "absolute"},
		{"unparseable retention", "test:\n  evidence:\n    retention: two weeks\n", "retention"},
		{"negative max_runs", "test:\n  evidence:\n    max_runs: -1\n", "max_runs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadGlobalFromBytes([]byte(tc.yaml)); err == nil {
				t.Fatal("expected the global config to fail closed")
			} else if !strings.Contains(err.Error(), tc.wantHint) {
				t.Fatalf("error %q does not name %q", err, tc.wantHint)
			}
			// The same value has to fail on a pushed branch too, so it cannot
			// merge and then surprise the next reader (see validateTestRaw).
			if _, err := LoadRepoFromBytes([]byte(tc.yaml)); err == nil {
				t.Fatal("expected the repository config to fail closed")
			}
		})
	}
}
