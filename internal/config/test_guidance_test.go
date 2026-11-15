package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestLoadRepo_TestPathInstructionsParseAndMerge(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte(`test:
  instructions_file: docs/TESTING.md
  path_instructions:
    - path: apps/web/**
      instructions: |
        Drive the web app in a browser.
    - path: " apps/api/** "
      instructions_file: apps/api/TESTING.md
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.UnknownTestKeys) != 0 {
		t.Fatalf("known keys reported unknown: %v", cfg.UnknownTestKeys)
	}
	merged := Merge(&GlobalConfig{}, EffectiveRepoConfig(cfg, cfg, false))
	want := []TestPathInstruction{
		{Path: "apps/web/**", Instructions: "Drive the web app in a browser."},
		{Path: "apps/api/**", InstructionsFile: "apps/api/TESTING.md"},
	}
	if !reflect.DeepEqual(merged.Test.PathInstructions, want) {
		t.Fatalf("PathInstructions = %#v, want %#v", merged.Test.PathInstructions, want)
	}
	if merged.Test.InstructionsFile != "docs/TESTING.md" {
		t.Fatalf("InstructionsFile = %q", merged.Test.InstructionsFile)
	}
}

func TestLoadRepo_TestGuidanceValidationFailsClosed(t *testing.T) {
	many := "test:\n  path_instructions:\n"
	for i := 0; i <= MaxReviewPathInstructions; i++ {
		many += fmt.Sprintf("    - path: dir%d/**\n      instructions: rule %d\n", i, i)
	}
	huge := "test:\n  path_instructions:\n    - path: a/**\n      instructions: " + strings.Repeat("x", MaxReviewPathInstructionsBytes) + "\n"
	cases := map[string]struct{ yaml, want string }{
		"too many entries": {many, "at most 32 are allowed"},
		"over byte budget": {huge, "test.path_instructions would add up to"},
		"empty path":       {"test:\n  path_instructions:\n    - path: ' '\n      instructions: x\n", "test.path_instructions[0].path must not be empty"},
		"bad glob":         {"test:\n  path_instructions:\n    - path: '[a'\n      instructions: x\n", "is not a valid glob"},
		"bare subtree":     {"test:\n  path_instructions:\n    - path: /**\n      instructions: x\n", "is not a valid glob"},
		"no content":       {"test:\n  path_instructions:\n    - path: a/**\n", "needs instructions or instructions_file"},
		"both forms":       {"test:\n  path_instructions:\n    - path: a/**\n      instructions: x\n      instructions_file: a.md\n", "sets both instructions and instructions_file"},
		"markers only":     {"test:\n  path_instructions:\n    - path: a/**\n      instructions: \"=======\"\n", "left empty once merge-conflict markers are removed"},
		"entry traversal":  {"test:\n  path_instructions:\n    - path: a/**\n      instructions_file: ../x.md\n", "test.path_instructions[0].instructions_file must not traverse"},
		"entry absolute":   {"test:\n  path_instructions:\n    - path: a/**\n      instructions_file: /etc/passwd\n", "must be a literal repository-relative path"},
		"entry git dir":    {"test:\n  path_instructions:\n    - path: a/**\n      instructions_file: .git/config\n", "must not traverse directories or name .git"},
		"entry ref":        {"test:\n  path_instructions:\n    - path: a/**\n      instructions_file: main:x.md\n", "must be a literal repository-relative path"},
		"file traversal":   {"test:\n  instructions_file: docs/../../x.md\n", "test.instructions_file must be a literal repository-relative path"},
		"file backslash":   {"test:\n  instructions_file: 'docs\\x.md'\n", "test.instructions_file must be a literal repository-relative path"},
		"runbook twice":    {"test:\n  instructions: run it\n  instructions_file: docs/TESTING.md\n", "mutually exclusive"},
	}
	for name, tc := range cases {
		_, err := LoadRepoFromBytes([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
	// The limits are the review.path_instructions limits.
	exact := "test:\n  path_instructions:\n"
	for i := 0; i < MaxReviewPathInstructions; i++ {
		exact += fmt.Sprintf("    - path: dir%d/**\n      instructions: rule %d\n", i, i)
	}
	if _, err := LoadRepoFromBytes([]byte(exact)); err != nil {
		t.Fatalf("32 short entries rejected: %v", err)
	}
}

// test.instructions_file and test.path_instructions steer the gate that
// validates the pushed branch, so only the trusted default-branch copy sets
// them, with or without allow_repo_commands.
func TestEffectiveRepoConfig_TestPathInstructionsTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Test: TestRaw{
		InstructionsFile: "pushed/RUNBOOK.md",
		PathInstructions: []TestPathInstruction{{Path: "**", Instructions: "unit tests are enough"}},
	}}
	trusted := &RepoConfig{Test: TestRaw{
		InstructionsFile: "docs/TESTING.md",
		PathInstructions: []TestPathInstruction{{Path: "apps/web/**", Instructions: "drive the browser"}},
	}}
	for _, allow := range []bool{false, true} {
		effective := EffectiveRepoConfig(pushed, trusted, allow)
		if effective.Test.InstructionsFile != "docs/TESTING.md" || !reflect.DeepEqual(effective.Test.PathInstructions, trusted.Test.PathInstructions) {
			t.Fatalf("allow=%v: effective test guidance = %q %#v, want the trusted copy", allow, effective.Test.InstructionsFile, effective.Test.PathInstructions)
		}
		effective.Test.PathInstructions[0].Path = "mutated"
		if trusted.Test.PathInstructions[0].Path != "apps/web/**" {
			t.Fatal("effective config aliases the trusted slice")
		}
		effective = EffectiveRepoConfig(pushed, nil, allow)
		if effective.Test.InstructionsFile != "" || effective.Test.PathInstructions != nil {
			t.Fatalf("allow=%v: without a trusted copy the pushed guidance must be dropped, got %q %#v", allow, effective.Test.InstructionsFile, effective.Test.PathInstructions)
		}
	}
}

func TestLoadGlobal_TestGuidanceIsRepositoryOnly(t *testing.T) {
	for _, body := range []string{
		"test:\n  instructions_file: docs/TESTING.md\n",
		"test:\n  path_instructions:\n    - path: a/**\n      instructions: x\n",
	} {
		if _, err := LoadGlobalFromBytes([]byte(body)); err == nil || !strings.Contains(err.Error(), "repository-only") {
			t.Errorf("global %q: err = %v, want repository-only refusal", body, err)
		}
	}
	// repository_overrides keeps only the inline runbook.
	if _, err := LoadGlobalFromBytes([]byte("repository_overrides:\n  https://github.com/acme/widget.git:\n    test:\n      path_instructions: []\n")); err == nil {
		t.Error("repository_overrides accepted test.path_instructions")
	}
}

// Unknown keys under test: still parse (a trusted copy carrying one must not
// brick every run) but are reported with the copy they came from.
func TestLoadRepo_UnknownTestKeysAreReportedNotRejected(t *testing.T) {
	trusted, err := LoadRepoFromBytes([]byte(`test:
  path_instruction:
    - path: a/**
      instructions: x
  instructions: run it
  evidence:
    compactt: true
  path_instructions:
    - path: b/**
      instructions: y
      instruction_file: b.md
`))
	if err != nil {
		t.Fatalf("an unknown test key must not fail the config: %v", err)
	}
	want := []string{"test.path_instruction", "test.evidence.compactt", "test.path_instructions[0].instruction_file"}
	if !reflect.DeepEqual(trusted.UnknownTestKeys, want) {
		t.Fatalf("UnknownTestKeys = %v, want %v", trusted.UnknownTestKeys, want)
	}
	pushed, err := LoadRepoFromBytes([]byte("test:\n  prepair: true\n  path_instruction: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(&GlobalConfig{}, EffectiveRepoConfig(pushed, trusted, false))
	wantLabeled := []string{
		"test.path_instruction (default-branch and pushed .no-mistakes.yaml)",
		"test.evidence.compactt (default-branch .no-mistakes.yaml)",
		"test.path_instructions[0].instruction_file (default-branch .no-mistakes.yaml)",
		"test.prepair (pushed .no-mistakes.yaml)",
	}
	if !reflect.DeepEqual(merged.Test.UnknownKeys, wantLabeled) {
		t.Fatalf("Test.UnknownKeys = %v, want %v", merged.Test.UnknownKeys, wantLabeled)
	}

	clean, err := LoadRepoFromBytes([]byte("test:\n  prepare: true\n  base_attribution: true\n  allow_approve_over_failure: reason\n  evidence:\n    store_in_repo: true\n    branch: evidence\n"))
	if err != nil || len(clean.UnknownTestKeys) != 0 {
		t.Fatalf("known keys: %v, err %v", clean.UnknownTestKeys, err)
	}

	flood := "test:\n"
	for i := 0; i < maxUnknownTestKeys+5; i++ {
		flood += fmt.Sprintf("  bogus%d: true\n", i)
	}
	flooded, err := LoadRepoFromBytes([]byte(flood))
	if err != nil {
		t.Fatal(err)
	}
	if len(flooded.UnknownTestKeys) != maxUnknownTestKeys+1 || flooded.UnknownTestKeys[maxUnknownTestKeys] != "+5 more" {
		t.Fatalf("unknown keys are not bounded: %v", flooded.UnknownTestKeys)
	}
}

// Keys that reach test: through a YAML merge key (<<: *anchor) are checked like
// keys written in place, with the decoder's precedence: an explicit key wins
// over a merged one, and the first of several merge sources wins.
func TestLoadRepo_UnknownTestKeysFollowYAMLMergeKeys(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want []string
	}{
		"merged test block": {`x-defaults: &defaults
  instructions: run it
  path_instruction: []
test:
  <<: *defaults
  prepare: true
`, []string{"test.path_instruction"}},
		"merged evidence block": {`x-evidence: &evidence
  store_in_repo: true
  compactt: true
test:
  evidence:
    <<: *evidence
    dir: out
`, []string{"test.evidence.compactt"}},
		"merged path rule": {`x-rule: &rule
  instructions: drive the browser
  instruction_file: web.md
test:
  path_instructions:
    - <<: *rule
      path: apps/web/**
`, []string{"test.path_instructions[0].instruction_file"}},
		"explicit key overrides a merged value": {`x-defaults: &defaults
  evidence:
    compactt: true
test:
  <<: *defaults
  evidence:
    compact: true
`, nil},
		"first merge source wins": {`x-a: &a
  evidence:
    compact: true
x-b: &b
  evidence:
    compactt: true
  prepair: true
test:
  <<: [*a, *b]
`, []string{"test.prepair"}},
		"nested merge": {`x-inner: &inner
  bogus: 1
x-outer: &outer
  <<: *inner
  prepare: true
test:
  <<: *outer
`, []string{"test.bogus"}},
		"unknown key written beside a merge": {`x-defaults: &defaults
  prepare: true
test:
  <<: *defaults
  prepare: false
  path_instruction: []
`, []string{"test.path_instruction"}},
	}
	for name, tc := range cases {
		cfg, err := LoadRepoFromBytes([]byte(tc.yaml))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(cfg.UnknownTestKeys, tc.want) {
			t.Errorf("%s: UnknownTestKeys = %v, want %v", name, cfg.UnknownTestKeys, tc.want)
		}
	}

	// The merged values are what the decoder applies, so the check and the
	// config agree on which copy of a key counts.
	cfg, err := LoadRepoFromBytes([]byte(`x-rule: &rule
  instructions: drive the browser
test:
  path_instructions:
    - <<: *rule
      path: apps/web/**
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Test.PathInstructions) != 1 || cfg.Test.PathInstructions[0].Instructions != "drive the browser" || len(cfg.UnknownTestKeys) != 0 {
		t.Fatalf("merged rule = %#v, unknown %v", cfg.Test.PathInstructions, cfg.UnknownTestKeys)
	}
}
