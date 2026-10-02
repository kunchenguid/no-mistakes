package quota

import (
	"strings"
	"testing"
	"time"
)

func intPtr(value int) *int { return &value }

func floatPtr(value float64) *float64 { return &value }

// measurableRow builds one catalog row with joined quota evidence.
func measurableRow(id string, class Class, percent int, priority float64, runway string, seconds int) ModelEvidence {
	row := ModelEvidence{
		ID:               id,
		Label:            id,
		Intelligence:     class,
		Measurable:       true,
		Scope:            "all_models",
		PercentRemaining: intPtr(percent),
		SpendPriority:    floatPtr(priority),
		RunwayStatus:     runway,
	}
	if seconds >= 0 {
		row.RunwaySeconds = intPtr(seconds)
	}
	return row
}

func freshProvider(name string, rows ...ModelEvidence) ProviderEvidence {
	return ProviderEvidence{Provider: name, State: providerStateFresh, Models: rows}
}

func reportOf(providers ...ProviderEvidence) Report {
	report := Report{Providers: make(map[string]ProviderEvidence, len(providers))}
	for _, provider := range providers {
		report.Providers[provider.Provider] = provider
	}
	return report
}

func TestParseReport_DecodesTheModelsJoin(t *testing.T) {
	const document = `{
	  "generatedAt": "2026-09-27T17:00:00Z",
	  "schemaVersion": 1,
	  "catalog": {"version": "2026-09-15", "provenance": "curated"},
	  "models": [
	    {"provider": "claude", "id": "claude-opus-4-5", "label": "Claude Opus 4.5", "intelligence": "high",
	     "quotaScopes": ["model:fable"],
	     "state": {"status": "fresh", "stale": false},
	     "effective": {"scope": "model:fable", "status": "known", "effectivePercentRemaining": 81,
	       "runway": {"status": "projected_exhaustion", "usableRunwaySeconds": 9301, "projectionConfidence": "early"},
	       "selection": {"status": "known", "spendPriority": -0.4755}}},
	    {"provider": "claude", "id": "claude-haiku-4-5", "label": "Claude Haiku 4.5", "intelligence": "medium",
	     "state": {"status": "fresh", "stale": false},
	     "effective": {"scope": "all_models", "status": "known", "effectivePercentRemaining": 88,
	       "runway": {"status": "through_reset"},
	       "selection": {"status": "known", "spendPriority": 0.0095}}},
	    {"provider": "grok", "id": "grok-4", "label": "Grok 4", "intelligence": "high",
	     "state": {"status": "auth_required", "stale": false}}
	  ]
	}`

	report, err := ParseReport([]byte(document))
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if got := report.GeneratedAt.UTC().Format(time.RFC3339); got != "2026-09-27T17:00:00Z" {
		t.Errorf("generatedAt = %q", got)
	}
	if report.CatalogVersion != "2026-09-15" {
		t.Errorf("catalog version = %q", report.CatalogVersion)
	}
	if got := report.ProviderNames(); strings.Join(got, ",") != "claude,grok" {
		t.Errorf("provider names = %v", got)
	}

	claude := report.Providers["claude"]
	if claude.Stale || claude.State != providerStateFresh {
		t.Errorf("claude state = %q stale=%v", claude.State, claude.Stale)
	}
	opus := claude.Models[0]
	if opus.Intelligence != ClassHigh || !opus.Measurable || opus.Scope != "model:fable" {
		t.Errorf("opus row = %+v", opus)
	}
	// The evidence is passed through verbatim: the routing signals are the tool's
	// numbers, not a re-derivation of them.
	if opus.PercentRemaining == nil || *opus.PercentRemaining != 81 {
		t.Errorf("opus percent = %v", opus.PercentRemaining)
	}
	if opus.SpendPriority == nil || *opus.SpendPriority != -0.4755 {
		t.Errorf("opus spend priority = %v", opus.SpendPriority)
	}
	if opus.RunwayStatus != RunwayProjectedExhaustion || opus.RunwaySeconds == nil || *opus.RunwaySeconds != 9301 {
		t.Errorf("opus runway = %q/%v", opus.RunwayStatus, opus.RunwaySeconds)
	}
	if opus.Confidence != "early" {
		t.Errorf("opus confidence = %q", opus.Confidence)
	}
	if haiku := claude.Models[1]; haiku.RunwayStatus != RunwayThroughReset || haiku.RunwaySeconds != nil {
		t.Errorf("haiku runway = %q/%v", haiku.RunwayStatus, haiku.RunwaySeconds)
	}
	// A catalog row with no joined evidence is evidence in itself: the provider is
	// set up on paper but has nothing measurable behind it.
	grok := report.Providers["grok"]
	if got := grok.Models[0].Measurable; got {
		t.Errorf("grok row measurable = %v, want false", got)
	}
	if ok, reason := grok.measurable(); ok || reason != "no measurable quota scope" {
		t.Errorf("grok measurable = %v/%q", ok, reason)
	}
	if ok, reason := grok.usable(); ok || reason != "provider state auth_required" {
		t.Errorf("grok usable = %v/%q", ok, reason)
	}
}

func TestParseReport_RejectsWhatCannotBeEvidence(t *testing.T) {
	for name, document := range map[string]string{
		"not json":   "quota-axi: no such flag",
		"not object": "[1, 2, 3]",
		"bad time":   `{"generatedAt": "yesterday", "models": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseReport([]byte(document)); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
	// An empty model list is a valid answer meaning "nothing here is
	// catalog-backed", not a malformed report.
	report, err := ParseReport([]byte(`{"models": []}`))
	if err != nil {
		t.Fatalf("empty report: %v", err)
	}
	if len(report.Providers) != 0 || report.Summary() != "no provider evidence" {
		t.Fatalf("empty report = %+v (%q)", report.Providers, report.Summary())
	}
}

func TestModelEvidence_FeasibleFailsClosed(t *testing.T) {
	budget := 30 * time.Minute
	cases := []struct {
		name     string
		row      ModelEvidence
		feasible bool
		want     string
	}{
		{name: "through reset has no deadline", row: ModelEvidence{RunwayStatus: RunwayThroughReset}, feasible: true, want: "runway through reset"},
		{name: "projected exhaustion covering the budget", row: ModelEvidence{RunwayStatus: RunwayProjectedExhaustion, RunwaySeconds: intPtr(3600)}, feasible: true, want: "covers the 30m step budget"},
		{name: "projected exhaustion shorter than the budget", row: ModelEvidence{RunwayStatus: RunwayProjectedExhaustion, RunwaySeconds: intPtr(600)}, feasible: false, want: "shorter than the 30m step budget"},
		{name: "zero runway is still exhaustion", row: ModelEvidence{RunwayStatus: RunwayProjectedExhaustion, RunwaySeconds: intPtr(0)}, feasible: false, want: "no usable runway"},
		{name: "exhausted now", row: ModelEvidence{RunwayStatus: RunwayExhaustedNow}, feasible: false, want: "quota exhausted now"},
		{name: "unknown runway is refused", row: ModelEvidence{RunwayStatus: RunwayUnknown}, feasible: false, want: "runway unknown"},
		{name: "missing runway is refused", row: ModelEvidence{}, feasible: false, want: "runway unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			feasible, reason := tc.row.Feasible(budget)
			if feasible != tc.feasible {
				t.Fatalf("feasible = %v, want %v (%s)", feasible, tc.feasible, reason)
			}
			if !strings.Contains(reason, tc.want) {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

func TestModelEvidence_BetterThanPrefersKnownSignals(t *testing.T) {
	rich := measurableRow("rich", ClassHigh, 90, 1.2, RunwayThroughReset, -1)
	poor := measurableRow("poor", ClassHigh, 20, -3, RunwayThroughReset, -1)
	if !rich.betterThan(poor) || poor.betterThan(rich) {
		t.Error("higher spend priority must outrank lower")
	}

	// An unmeasurable signal never outranks a measured one: a row whose priority
	// the tool could not compute is not evidence of a better place to spend.
	unmeasurable := measurableRow("unmeasurable", ClassHigh, 0, 0, RunwayThroughReset, -1)
	unmeasurable.SpendPriority = nil
	unmeasurable.PercentRemaining = nil
	if unmeasurable.betterThan(poor) {
		t.Error("a row with no routing signals must not outrank a measured row")
	}
	if !poor.betterThan(unmeasurable) {
		t.Error("a measured row must outrank one with no routing signals")
	}

	// Equal signals leave the decision to the configured candidate order.
	equal := measurableRow("equal", ClassHigh, 20, -3, RunwayThroughReset, -1)
	if equal.betterThan(poor) || poor.betterThan(equal) {
		t.Error("equal standings must not prefer either row")
	}

	hidden := measurableRow("hidden", ClassHigh, 20, -3, RunwayUnknown, -1)
	if hidden.betterThan(equal) || equal.betterThan(hidden) {
		t.Error("runway length must not decide between rows that share the other signals")
	}
}
