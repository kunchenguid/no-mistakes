package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestQuotaRoutingOffByDefault pins the opt-in default: without the block the
// feature is off and the knobs carry their documented defaults, and the merge
// into an effective config keeps them global-only.
func TestQuotaRoutingOffByDefault(t *testing.T) {
	cfg, err := LoadGlobal(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaRouting.Enabled {
		t.Fatal("quota_routing.enabled = true by default, want the feature opt-in")
	}
	if cfg.QuotaRouting.ExpectedRunLength != DefaultQuotaExpectedRunLength {
		t.Fatalf("expected_run_length = %s, want %s", cfg.QuotaRouting.ExpectedRunLength, DefaultQuotaExpectedRunLength)
	}
	if cfg.QuotaRouting.ReadTimeout != DefaultQuotaReadTimeout {
		t.Fatalf("read_timeout = %s, want %s", cfg.QuotaRouting.ReadTimeout, DefaultQuotaReadTimeout)
	}
	if cfg.QuotaRouting.QuotaAXIPath != "" {
		t.Fatalf("quota_axi_path = %q, want empty (quota-axi on PATH)", cfg.QuotaRouting.QuotaAXIPath)
	}
	merged := Merge(cfg, &RepoConfig{})
	if merged.QuotaRouting != cfg.QuotaRouting {
		t.Fatalf("merged quota_routing = %#v, want the global values", merged.QuotaRouting)
	}
}

func TestQuotaRoutingSettingsAreConfigurable(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("quota_routing:\n  enabled: true\n  expected_run_length: 2h\n  read_timeout: 30s\n  quota_axi_path: /usr/local/bin/quota-axi\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.QuotaRouting.Enabled {
		t.Fatal("quota_routing.enabled = false, want true")
	}
	if cfg.QuotaRouting.ExpectedRunLength != 2*time.Hour {
		t.Fatalf("expected_run_length = %s, want 2h", cfg.QuotaRouting.ExpectedRunLength)
	}
	if cfg.QuotaRouting.ReadTimeout != 30*time.Second {
		t.Fatalf("read_timeout = %s, want 30s", cfg.QuotaRouting.ReadTimeout)
	}
	if cfg.QuotaRouting.QuotaAXIPath != "/usr/local/bin/quota-axi" {
		t.Fatalf("quota_axi_path = %q, want the configured path", cfg.QuotaRouting.QuotaAXIPath)
	}
	merged := Merge(cfg, &RepoConfig{})
	if !merged.QuotaRouting.Enabled {
		t.Fatal("merged quota_routing.enabled = false, want the global true carried through")
	}
}

func TestQuotaRoutingRejectsBadDurations(t *testing.T) {
	for name, doc := range map[string]string{
		"zero expected run length":  "quota_routing:\n  enabled: true\n  expected_run_length: 0s\n",
		"negative expected run":     "quota_routing:\n  enabled: true\n  expected_run_length: -5m\n",
		"zero read timeout":         "quota_routing:\n  enabled: true\n  read_timeout: 0s\n",
		"negative read timeout":     "quota_routing:\n  enabled: true\n  read_timeout: -1s\n",
		"unparseable run length":    "quota_routing:\n  enabled: true\n  expected_run_length: soon\n",
		"empty quota_axi_path only": "quota_routing:\n  enabled: true\n  quota_axi_path: \"  \"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadGlobalFromBytes([]byte(doc)); err == nil {
				t.Fatalf("LoadGlobalFromBytes(%q) = nil error, want a config refusal", doc)
			}
		})
	}
}

// TestQuotaRoutingIsRepositoryOnlyRefused pins the trust boundary: a pushed
// branch must never be able to reorder this machine's agent list, so the key
// cannot even appear in repository configuration.
func TestQuotaRoutingIsRepositoryOnlyRefused(t *testing.T) {
	_, err := LoadRepoFromBytes([]byte("quota_routing:\n  enabled: true\n"))
	if err == nil || !strings.Contains(err.Error(), "quota_routing") {
		t.Fatalf("repo config error = %v, want a refusal naming quota_routing", err)
	}
}

func TestQuotaProviderOnAgentConfig(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("agent_config:\n  opencode:\n    quota_provider: openrouter\n  pi:\n    model: codex-native/gpt-5.4\n    quota_provider: codex\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.QuotaProviderFor("opencode"); got != "openrouter" {
		t.Fatalf("opencode quota provider = %q, want openrouter", got)
	}
	if got := cfg.QuotaProviderFor("pi"); got != "codex" {
		t.Fatalf("pi quota provider = %q, want codex (a declared provider wins over the model prefix)", got)
	}
	// A profile that declares only a quota provider still records the provider
	// even though the harness profile itself is empty.
	if got := cfg.QuotaProviderFor("codex"); got != "" {
		t.Fatalf("codex quota provider = %q, want empty when not declared", got)
	}
	merged := Merge(cfg, &RepoConfig{})
	if got := merged.QuotaProviderFor("opencode"); got != "openrouter" {
		t.Fatalf("merged opencode quota provider = %q, want openrouter", got)
	}
}

func TestQuotaProviderMustNameARealAgent(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("agent_config:\n  not-an-agent:\n    quota_provider: openrouter\n")); err == nil {
		t.Fatal("agent_config with an unknown agent name = nil error, want a refusal")
	}
	if _, err := LoadGlobalFromBytes([]byte("agent_config:\n  opencode:\n    quota_provider: \"  \"\n")); err == nil {
		t.Fatal("empty quota_provider = nil error, want a refusal")
	}
}

// TestQuotaProviderSurvivesOnAnOtherwiseEmptyProfile pins that a
// quota_provider-only entry is not dropped by the zero-profile filter.
func TestQuotaProviderSurvivesOnAnOtherwiseEmptyProfile(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("quota_routing:\n  enabled: true\nagent_config:\n  grok:\n    quota_provider: openrouter\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.QuotaProviderFor("grok"); got != "openrouter" {
		t.Fatalf("grok quota provider = %q, want openrouter", got)
	}
	if cfg.AgentConfig["grok"].Model != "" {
		t.Fatalf("grok model = %q, want empty", cfg.AgentConfig["grok"].Model)
	}
}

func TestQuotaProviderUsesExactFirstBidirectionalACPAliases(t *testing.T) {
	for _, tc := range []struct{ selected, configured string }{
		{"cursor", "acp:cursor"}, {"acp:cursor", "cursor"},
		{"devin", "acp:devin"}, {"acp:devin", "devin"},
	} {
		for _, exact := range []bool{false, true} {
			data := "agent: " + tc.selected + "\nagent_config:\n  " + tc.configured + ": {quota_provider: shared-provider}\n"
			want := "shared-provider"
			if exact {
				data += "  " + tc.selected + ": {quota_provider: exact-provider}\n"
				want = "exact-provider"
			}
			global, err := LoadGlobalFromBytes([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			effective := Merge(global, &RepoConfig{})
			role := effective.ForReviewAgent(ReviewAgent{Agent: types.AgentName(tc.selected)})
			for surface, got := range map[string]string{
				"global":      global.QuotaProviderFor(types.AgentName(tc.selected)),
				"effective":   effective.QuotaProviderFor(types.AgentName(tc.selected)),
				"review role": role.QuotaProviderFor(types.AgentName(tc.selected)),
			} {
				if got != want {
					t.Fatalf("selected=%s configured=%s exact=%v %s provider=%q want=%q", tc.selected, tc.configured, exact, surface, got, want)
				}
			}
		}
	}
}
