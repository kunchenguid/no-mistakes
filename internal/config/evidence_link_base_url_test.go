package config

import (
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestEvidenceLinkBaseURLIsGlobalOnly is the trust-boundary regression for
// test.evidence.link_base_url: it decides where every PR body's evidence links
// point, so like local_root it resolves from GlobalConfig alone. A repository
// value, trusted copy included, loads without error and is ignored.
func TestEvidenceLinkBaseURLIsGlobalOnly(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("test:\n  evidence:\n    link_base_url: https://evidence.example.com/no-mistakes/\n"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte("test:\n  evidence:\n    link_base_url: https://attacker.example/evidence\n"))
	if err != nil {
		t.Fatalf("a repository link_base_url must load and be ignored, not refused: %v", err)
	}
	effective := EffectiveRepoConfig(repo, repo, true)

	if got := Merge(global, effective).Test.Evidence.LinkBaseURL; got != "https://evidence.example.com/no-mistakes" {
		t.Errorf("link_base_url = %q, want the global value without its trailing slash", got)
	}
	if got := Merge(DefaultGlobalConfig(), effective).Test.Evidence.LinkBaseURL; got != "" {
		t.Errorf("link_base_url = %q, want empty: a repository must not fill the gap", got)
	}
	if got := Merge(DefaultGlobalConfig(), &RepoConfig{}).Test.Evidence.LinkBaseURL; got != "" {
		t.Errorf("default link_base_url = %q, want unset", got)
	}
}

// TestRepoConfigProvenanceRoundTripAcceptsEvidenceLinkBaseURL: a review round
// records yaml.Marshal of the effective RepoConfig as its provenance, and eval
// capture and the carried-review check read it back through LoadRepoFromBytes.
// That record names every raw field, so refusing the global-only key by name
// at parse time broke both for every run. The key must round-trip.
func TestRepoConfigProvenanceRoundTripAcceptsEvidenceLinkBaseURL(t *testing.T) {
	for _, src := range []string{"", "test:\n  evidence:\n    link_base_url: https://attacker.example/evidence\n"} {
		repo, err := LoadRepoFromBytes([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		recorded, err := yaml.Marshal(repo)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(recorded), "link_base_url") {
			t.Fatalf("the provenance record omits link_base_url, so this test proves nothing:\n%s", recorded)
		}
		if _, err := LoadRepoFromBytes(recorded); err != nil {
			t.Fatalf("recorded provenance %q no longer loads: %v", src, err)
		}
	}
}

func TestLoadConfig_EvidenceLinkBaseURLValidation(t *testing.T) {
	accepted := map[string]string{
		"": "",
		"https://evidence.example.com/no-mistakes":       "https://evidence.example.com/no-mistakes",
		"http://127.0.0.1:8765/":                         "http://127.0.0.1:8765",
		"  https://evidence.example.com/no-mistakes//  ": "https://evidence.example.com/no-mistakes",
	}
	for raw, want := range accepted {
		global, err := LoadGlobalFromBytes([]byte("test:\n  evidence:\n    link_base_url: " + strconv.Quote(raw) + "\n"))
		if err != nil {
			t.Fatalf("%q was refused: %v", raw, err)
		}
		if got := Merge(global, &RepoConfig{}).Test.Evidence.LinkBaseURL; got != want {
			t.Errorf("%q resolved to %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{
		"file:///srv/evidence",
		"evidence.example.com/no-mistakes",
		"ftp://evidence.example.com/no-mistakes",
		"https://user:secret@evidence.example.com/no-mistakes",
		"https://evidence.example.com/no-mistakes?run=1",
		"https://evidence.example.com/no-mistakes#top",
		"https://evidence.example.com/no mistakes",
	} {
		doc := []byte("test:\n  evidence:\n    link_base_url: " + strconv.Quote(raw) + "\n")
		if _, err := LoadGlobalFromBytes(doc); err == nil || !strings.Contains(err.Error(), "link_base_url") {
			t.Errorf("global %q: err = %v, want a link_base_url refusal", raw, err)
		}
		// The pushed copy fails the same way, like local_root: an invalid
		// value is never honored, but it must not merge either.
		if _, err := LoadRepoFromBytes(doc); err == nil || !strings.Contains(err.Error(), "link_base_url") {
			t.Errorf("repo %q: err = %v, want a link_base_url refusal", raw, err)
		}
	}
}
