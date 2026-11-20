package config

import (
	"strings"
	"testing"
)

const externalPublisherConfig = `push:
  external_publisher:
    paths: [.shipping-gate.json]
    reason: The external gate owns publication.
    remedy: Publish through that gate.
`

func TestExternalPublisherPolicy_TrustedOnly(t *testing.T) {
	t.Parallel()
	for _, optIn := range []bool{false, true} {
		trusted, err := LoadRepoFromBytes([]byte(externalPublisherConfig))
		if err != nil {
			t.Fatal(err)
		}
		pushed, err := LoadRepoFromBytes([]byte("push: {}\n"))
		if err != nil {
			t.Fatal(err)
		}
		global, err := LoadGlobalFromBytes([]byte("{}\n"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := Merge(global, EffectiveRepoConfig(pushed, trusted, optIn))
		if got := cfg.Push.ExternalPublisher; got == nil || got.Reason != "The external gate owns publication." || len(got.Paths) != 1 || got.Paths[0] != ".shipping-gate.json" {
			t.Fatalf("trusted policy lost with commands opt-in=%v: %+v", optIn, got)
		}
		cfg = Merge(global, EffectiveRepoConfig(trusted, nil, optIn))
		if cfg.Push.ExternalPublisher != nil {
			t.Fatalf("pushed policy applied without a trusted declaration, commands opt-in=%v", optIn)
		}
	}
}

func TestExternalPublisherPolicy_MalformedFailsClosed(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"push: false\n",
		"push: {external_publishers: {}}\n",
		"push: {external_publisher: false}\n",
		"push: {external_publisher: {}}\n",
		strings.Replace(externalPublisherConfig, "paths:", "path:", 1),
		strings.Replace(externalPublisherConfig, "paths: [.shipping-gate.json]", "paths: []", 1),
		strings.Replace(externalPublisherConfig, "reason: The external gate owns publication.", "reason: ' '", 1),
		strings.Replace(externalPublisherConfig, "remedy: Publish through that gate.", "remedy: ''", 1),
	} {
		if _, err := LoadRepoFromBytes([]byte(raw)); err == nil {
			t.Errorf("malformed policy accepted: %s", raw)
		}
	}
	for _, name := range []string{"", ".", "..", "../gate", "/gate", "a/../gate", "a//gate", `a\gate`, "gate\nfile"} {
		p := Push{ExternalPublisher: &ExternalPublisher{Paths: []string{name}, Reason: "reason", Remedy: "remedy"}}
		if err := p.Validate(); err == nil {
			t.Errorf("unsafe path %q accepted", name)
		}
	}
}
