package routing

import "testing"

func TestDecideBootstrapsReviewOnLuna(t *testing.T) {
	d := Decide(Input{Harness: "codex", Purpose: "review", Risk: RiskUnknown, Repository: "https://GitHub.com/RaFoyer/no-mistakes.git"})
	if d.EffectiveModel != ModelLuna || d.EffectiveEffort != EffortXHigh {
		t.Fatalf("initial review route = %s/%s, want Luna/xhigh", d.EffectiveModel, d.EffectiveEffort)
	}
	if d.Phase != "review" || d.Repository != "github.com/rafoyer/no-mistakes" {
		t.Fatalf("route metadata = %+v", d)
	}
}

func TestDecideUsesTerraForMediumHighNonReviewWork(t *testing.T) {
	d := Decide(Input{Harness: "codex", Purpose: "test-evidence", Risk: RiskMedium})
	if d.EffectiveModel != ModelTerra || d.EffectiveEffort != EffortHigh {
		t.Fatalf("medium-risk route = %s/%s, want Terra/high", d.EffectiveModel, d.EffectiveEffort)
	}
}

func TestDecideAllowsSolOnlyAfterHighRiskReviewClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     Input
		want   string
		effort string
	}{
		{"first high-risk review", Input{Harness: "codex", Purpose: "review", Risk: RiskHigh}, ModelLuna, EffortXHigh},
		{"high-risk fixer", Input{Harness: "codex", Purpose: "review-fix", Risk: RiskHigh}, ModelTerra, EffortHigh},
		{"confirmation", ReviewConfirmation(Input{Harness: "codex", Risk: RiskHigh}), ModelSol, EffortHigh},
		{"test never inherits Sol", Input{Harness: "codex", Purpose: "test", Risk: RiskHigh, ReviewConfirmation: true}, ModelTerra, EffortHigh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.in)
			if got.EffectiveModel != tc.want {
				t.Fatalf("route = %s/%s, want %s", got.EffectiveModel, got.EffectiveEffort, tc.want)
			}
			wantEffort := tc.effort
			if wantEffort == "" {
				wantEffort = EffortXHigh
			}
			if got.EffectiveEffort != wantEffort {
				t.Fatalf("effort = %s, want %s", got.EffectiveEffort, wantEffort)
			}
		})
	}
}

func TestConfigFingerprintIsStreamingAndDeterministic(t *testing.T) {
	a := ConfigFingerprint(string(make([]byte, 10_000)))
	b := ConfigFingerprint(string(make([]byte, 10_000)))
	if a == "" || a != b || len(a) != 24 {
		t.Fatalf("fingerprint = %q, want deterministic 24 hex chars", a)
	}
	changedTail := string(make([]byte, 10_000))
	changedTail = changedTail[:9_999] + "x"
	if a == ConfigFingerprint(changedTail) {
		t.Fatal("fingerprint ignored a tail configuration change")
	}
	changedMiddle := string(make([]byte, 10_000))
	changedMiddle = changedMiddle[:5_000] + "x" + changedMiddle[5_001:]
	if a == ConfigFingerprint(changedMiddle) {
		t.Fatal("same-length middle configuration edit did not change fingerprint")
	}
	if got := CanonicalRepository("git@GitHub.com:RaFoyer/No-Mistakes.git"); got != "github.com/rafoyer/no-mistakes" {
		t.Fatalf("canonical repository = %q", got)
	}
}

func TestDecideDoesNotClaimGPTRouteForNonCodexHarness(t *testing.T) {
	d := Decide(Input{Harness: "claude", Purpose: "review", Risk: RiskUnknown})
	if d.EffectiveModel != "" || d.EffectiveEffort != "" {
		t.Fatalf("non-Codex route = %s/%s, want no unreceived GPT controls", d.EffectiveModel, d.EffectiveEffort)
	}
}

func TestDecideBoundsAndValidatesUntrustedRouteMetadata(t *testing.T) {
	d := Decide(Input{
		Harness:    string(make([]byte, 10_000)),
		Risk:       Risk("unexpected-risk"),
		Repository: "https://example.com/" + string(make([]byte, 10_000)),
		Purpose:    "test",
	})
	if d.Risk != RiskUnknown {
		t.Fatalf("risk = %q, want unknown", d.Risk)
	}
	if len(d.RequestedHarness) > 128 || len(d.Repository) > 512 {
		t.Fatalf("untrusted route metadata was not bounded: harness=%d repository=%d", len(d.RequestedHarness), len(d.Repository))
	}
}
