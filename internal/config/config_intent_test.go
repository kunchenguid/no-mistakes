package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntentDefaults(t *testing.T) {
	got := intentDefaults()
	if got.Check.Mode != "" && got.Check.Mode != IntentCheckOff {
		t.Errorf("default intent check mode = %q, want off", got.Check.Mode)
	}
	if !got.Enabled {
		t.Error("default Enabled should be true (opt-out)")
	}
	if got.Threshold != 0.2 {
		t.Errorf("default Threshold = %v, want 0.2", got.Threshold)
	}
	if got.SlackDays != 3 {
		t.Errorf("default SlackDays = %d, want 3", got.SlackDays)
	}
}

func TestIntentMerge_GlobalDisable(t *testing.T) {
	disabled := false
	global := &GlobalConfig{Intent: GlobalIntentRaw{IntentRaw: IntentRaw{Enabled: &disabled}}}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)
	if cfg.Intent.Enabled {
		t.Error("global disable should propagate")
	}
	// Defaults preserved for other fields.
	if cfg.Intent.SlackDays != 3 {
		t.Errorf("slack days = %d, want default 3", cfg.Intent.SlackDays)
	}
}

func TestIntentMerge_RepoOverridesGlobal(t *testing.T) {
	enabled := true
	disabled := false
	threshold := 0.5
	global := &GlobalConfig{Intent: GlobalIntentRaw{IntentRaw: IntentRaw{Enabled: &disabled}}}
	repo := &RepoConfig{Intent: IntentRaw{Enabled: &enabled, Threshold: &threshold}}

	cfg := Merge(global, repo)
	if !cfg.Intent.Enabled {
		t.Error("repo enable should override global disable")
	}
	if cfg.Intent.Threshold != 0.5 {
		t.Errorf("threshold = %v, want 0.5", cfg.Intent.Threshold)
	}
}

func TestIntentMerge_DisabledReadersAccumulate(t *testing.T) {
	global := &GlobalConfig{Intent: GlobalIntentRaw{IntentRaw: IntentRaw{DisabledReaders: []string{"codex"}}}}
	repo := &RepoConfig{Intent: IntentRaw{DisabledReaders: []string{" Rovodev "}}}

	cfg := Merge(global, repo)
	if !cfg.Intent.DisabledReaders["codex"] {
		t.Error("codex should be disabled from global")
	}
	if !cfg.Intent.DisabledReaders["rovodev"] {
		t.Error("rovodev (normalized) should be disabled from repo")
	}
}

func TestLoadGlobalConfig_IntentParsed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
agent: claude
intent:
  enabled: false
  threshold: 0.4
  slack_days: 7
  disabled_readers:
    - codex
    - opencode
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Intent.Enabled == nil || *cfg.Intent.Enabled {
		t.Error("expected Enabled=false")
	}
	if cfg.Intent.Threshold == nil || *cfg.Intent.Threshold != 0.4 {
		t.Error("expected Threshold=0.4")
	}
	if cfg.Intent.SlackDays == nil || *cfg.Intent.SlackDays != 7 {
		t.Error("expected SlackDays=7")
	}
	if len(cfg.Intent.DisabledReaders) != 2 {
		t.Errorf("disabled_readers count = %d, want 2", len(cfg.Intent.DisabledReaders))
	}
}

func TestLoadRepoConfig_IntentParsed(t *testing.T) {
	dir := t.TempDir()
	yaml := `
intent:
  enabled: false
`
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Intent.Enabled == nil || *cfg.Intent.Enabled {
		t.Error("expected repo Enabled=false")
	}
}

func TestGlobalIntentPublishIntentDefaults(t *testing.T) {
	var unset GlobalIntentRaw
	if got := unset.PublishesIntentByDefault(); !got {
		t.Error("unset publish_intent should default to publishing (opt-out)")
	}
	publish := false
	withPublish := GlobalIntentRaw{PublishIntent: &publish}
	if got := withPublish.PublishesIntentByDefault(); got {
		t.Error("publish_intent: false should suppress publication by default")
	}
	doNotPublish := true
	withoutPublish := GlobalIntentRaw{PublishIntent: &doNotPublish}
	if got := withoutPublish.PublishesIntentByDefault(); !got {
		t.Error("publish_intent: true should publish by default")
	}
}

func TestLoadGlobalConfig_IntentPublishIntentParsed(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want *bool
	}{
		{name: "absent defaults to publish", yaml: "agent: claude\n", want: nil},
		{name: "false suppresses publication", yaml: "intent:\n  publish_intent: false\n", want: boolPtr(false)},
		{name: "true publishes", yaml: "intent:\n  publish_intent: true\n", want: boolPtr(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			cfg, err := LoadGlobal(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if (cfg.Intent.PublishIntent == nil) != (tc.want == nil) {
				t.Fatalf("PublishIntent = %#v, want %#v", cfg.Intent.PublishIntent, tc.want)
			}
			if tc.want != nil && *cfg.Intent.PublishIntent != *tc.want {
				t.Fatalf("PublishIntent = %v, want %v", *cfg.Intent.PublishIntent, *tc.want)
			}
			if got := cfg.Intent.PublishesIntentByDefault(); got != (tc.want == nil || *tc.want) {
				t.Fatalf("PublishesIntentByDefault() = %v", got)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestIntentCheckDefaultsOffAndPoliciesComposeAdditively(t *testing.T) {
	defaultConfig := Merge(DefaultGlobalConfig(), &RepoConfig{})
	if defaultConfig.Intent.Check.Mode != IntentCheckOff || defaultConfig.PR.Intent.Format != "" {
		t.Fatalf("new behavior should be off by default: intent=%#v PR=%#v", defaultConfig.Intent.Check, defaultConfig.PR.Intent)
	}
	global, err := LoadGlobalFromBytes([]byte("intent:\n  check:\n    mode: reject\n    terms: [operator-only]\n    patterns: ['(?i)run-[0-9]+']\n"))
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := LoadRepoFromBytes([]byte("intent:\n  check:\n    mode: reject\n    terms: [attacker-rule]\n"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte("intent:\n  check:\n    mode: warn\n    terms: [repository-rule]\n"))
	if err != nil {
		t.Fatal(err)
	}
	effective := EffectiveRepoConfig(pushed, trusted, false)
	check := Merge(global, effective).Intent.Check
	if check.Mode != IntentCheckReject {
		t.Fatalf("mode = %q, want strongest configured reject mode", check.Mode)
	}
	for text, want := range map[string]bool{
		"OPERATOR-ONLY":   true,
		"run-42":          true,
		"Repository-Rule": true,
		"attacker-rule":   false,
	} {
		if got := len(check.Match(text)) > 0; got != want {
			t.Errorf("Match(%q) = %v, want %v", text, got, want)
		}
	}
	if got := check.Match("run-42"); len(got) != 1 || got[0] != "patterns[0]" {
		t.Fatalf("pattern result = %v; matched content must not be returned", got)
	}
}

func TestIntentCheckPushedPolicyDoesNotApplyWithoutTrustedCopy(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte("intent:\n  check:\n    mode: reject\n    terms: [must-not-apply]\n"))
	if err != nil {
		t.Fatal(err)
	}
	effective := EffectiveRepoConfig(pushed, nil, false)
	check := Merge(DefaultGlobalConfig(), effective).Intent.Check
	if check.Mode != IntentCheckOff || len(check.Match("must-not-apply")) != 0 {
		t.Fatalf("pushed policy took effect: %#v", check)
	}
}

func TestPRIntentFormatAndSectionsAreTrustedOnly(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte("pr:\n  intent:\n    format: brief\n    sections: [why]\n"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte("pr:\n  intent:\n    format: brief\n    sections: [scope, out_of_scope]\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, allowRepoCommands := range []bool{false, true} {
		effective := EffectiveRepoConfig(pushed, trusted, allowRepoCommands)
		got := Merge(DefaultGlobalConfig(), effective).PR.Intent
		if got.Format != "brief" || len(got.Sections) != 2 || got.Sections[0] != "scope" || got.Sections[1] != "out_of_scope" {
			t.Fatalf("allow_repo_commands=%v: PR intent = %#v, want trusted settings", allowRepoCommands, got)
		}
	}
	effective := EffectiveRepoConfig(pushed, nil, false)
	if got := Merge(DefaultGlobalConfig(), effective).PR.Intent; got.Format != "" || len(got.Sections) != 0 {
		t.Fatalf("untrusted PR intent config took effect: %#v", got)
	}
}

func TestIntentConfigRejectsInvalidPolicies(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "invalid mode", data: "intent:\n  check:\n    mode: redact\n"},
		{name: "invalid regex", data: "intent:\n  check:\n    patterns: ['(']\n"},
		{name: "oversized rule", data: "intent:\n  check:\n    terms: ['" + strings.Repeat("x", maxIntentCheckRuleLength+1) + "']\n"},
		{name: "invalid brief format", data: "pr:\n  intent:\n    format: layout\n"},
		{name: "unknown brief section", data: "pr:\n  intent:\n    sections: [audience]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadRepoFromBytes([]byte(tc.data)); err == nil {
				t.Fatal("invalid intent config unexpectedly loaded")
			}
		})
	}
}

// The global publication default must live only in global config. The repo
// raw type has no publication key (compile-enforced), so a pushed branch's
// .no-mistakes.yaml writing intent.publish_intent is silently ignored and the
// operator's global default always wins. This test pins the observable half:
// the ignored key parses without error and leaves the resolved defaults.
func TestRepoConfigCannotExpressIntentPublishIntent(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("intent:\n  publish_intent: false\n"))
	if err != nil {
		t.Fatalf("repo yaml with the global-only publication key must parse and be ignored: %v", err)
	}
	cfg := Merge(DefaultGlobalConfig(), repo)
	if !cfg.Intent.Enabled || cfg.Intent.Threshold != 0.2 || cfg.Intent.SlackDays != 3 {
		t.Fatalf("ignored publication key changed resolved intent defaults: %+v", cfg.Intent)
	}
}
