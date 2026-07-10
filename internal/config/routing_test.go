package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDefaultRoutingResolvesSixProfilesExactly(t *testing.T) {
	rc := DefaultRoutingConfig()
	want := map[ProfileName][]Candidate{
		ProfileFixFast: {
			{Runner: types.RunnerCodex, Model: "gpt-5.6-luna", Effort: types.EffortMedium},
			{Runner: types.RunnerClaude, Model: "claude-sonnet-5", Effort: types.EffortMedium},
		},
		ProfileProseFast: {
			{Runner: types.RunnerCodex, Model: "gpt-5.6-luna", Effort: types.EffortLow},
			{Runner: types.RunnerClaude, Model: "claude-sonnet-5", Effort: types.EffortLow},
		},
		ProfileFixBalanced: {
			{Runner: types.RunnerCodex, Model: "gpt-5.6-terra", Effort: types.EffortMedium},
			{Runner: types.RunnerClaude, Model: "claude-opus-4-8", Effort: types.EffortMedium},
		},
		ProfileToolsBalanced: {
			{Runner: types.RunnerCodex, Model: "gpt-5.6-terra", Effort: types.EffortHigh},
			{Runner: types.RunnerClaude, Model: "claude-opus-4-8", Effort: types.EffortHigh},
		},
		ProfileReviewStrong: {
			{Runner: types.RunnerCodex, Model: "gpt-5.6-sol", Effort: types.EffortHigh},
			{Runner: types.RunnerClaude, Model: "claude-fable-5", Effort: types.EffortHigh},
		},
		ProfileAuthorityStrong: {
			{Runner: types.RunnerCodex, Model: "gpt-5.6-sol", Effort: types.EffortXHigh},
			{Runner: types.RunnerClaude, Model: "claude-fable-5", Effort: types.EffortXHigh},
		},
	}
	if len(rc.Profiles) != len(want) {
		t.Fatalf("default profiles = %d, want %d", len(rc.Profiles), len(want))
	}
	for name, candidates := range want {
		p, ok := rc.Profiles[name]
		if !ok {
			t.Fatalf("missing default profile %q", name)
		}
		if p.Name != name {
			t.Fatalf("profile %q Name field = %q", name, p.Name)
		}
		if !reflect.DeepEqual(p.Candidates, candidates) {
			t.Fatalf("profile %q candidates = %+v, want %+v", name, p.Candidates, candidates)
		}
	}
}

func TestDefaultRoutingValidates(t *testing.T) {
	if err := DefaultRoutingConfig().Validate(); err != nil {
		t.Fatalf("DefaultRoutingConfig().Validate() = %v, want nil", err)
	}
}

func TestDefaultRoutingRunnersMapProviderDomains(t *testing.T) {
	rc := DefaultRoutingConfig()
	if rc.Runners[types.RunnerCodex].FailureDomain != types.FailureDomainOpenAI {
		t.Fatalf("codex failure domain = %q", rc.Runners[types.RunnerCodex].FailureDomain)
	}
	if rc.Runners[types.RunnerClaude].FailureDomain != types.FailureDomainAnthropic {
		t.Fatalf("claude failure domain = %q", rc.Runners[types.RunnerClaude].FailureDomain)
	}
}

func TestDefaultRoutingHasNoProSelectors(t *testing.T) {
	for name, p := range DefaultRoutingConfig().Profiles {
		for _, c := range p.Candidates {
			if strings.HasSuffix(c.Model, "-pro") {
				t.Fatalf("default profile %q includes Pro selector %q", name, c.Model)
			}
		}
	}
}

func TestDefaultRoutingCoversEveryRegisteredPurpose(t *testing.T) {
	rc := DefaultRoutingConfig()
	for _, def := range types.AllPurposeDefinitions() {
		profiles, err := rc.ResolveRoute(def.Purpose)
		if err != nil {
			t.Fatalf("ResolveRoute(%q) = %v", def.Purpose, err)
		}
		if len(profiles) == 0 {
			t.Fatalf("purpose %q resolved to an empty cascade", def.Purpose)
		}
	}
}

func TestResolveRouteReturnsOrderedCascade(t *testing.T) {
	rc := DefaultRoutingConfig()
	profiles, err := rc.ResolveRoute(types.PurposeStructuredFindingRepair)
	if err != nil {
		t.Fatalf("ResolveRoute: %v", err)
	}
	gotNames := make([]ProfileName, len(profiles))
	for i, p := range profiles {
		gotNames[i] = p.Name
	}
	want := []ProfileName{ProfileFixFast, ProfileFixBalanced, ProfileAuthorityStrong}
	if !reflect.DeepEqual(gotNames, want) {
		t.Fatalf("structured repair cascade = %v, want %v", gotNames, want)
	}
	single, err := rc.ResolveRoute(types.PurposeInitialReview)
	if err != nil {
		t.Fatalf("ResolveRoute initial review: %v", err)
	}
	if len(single) != 1 || single[0].Name != ProfileReviewStrong {
		t.Fatalf("initial review route = %v, want [review_strong]", single)
	}
}

func TestRoutingValidateAllowsProModelsInCustomProfile(t *testing.T) {
	rc := DefaultRoutingConfig()
	p := rc.Profiles[ProfileReviewStrong]
	p.Candidates = append([]Candidate(nil), p.Candidates...)
	p.Candidates[0].Model = "gpt-5.6-sol-pro"
	rc.Profiles[ProfileReviewStrong] = p
	if err := rc.Validate(); err != nil {
		t.Fatalf("Pro model rejected by validation: %v", err)
	}
}

func TestRoutingValidateRejectsBadConfigs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RoutingConfig)
	}{
		{"unsupported runner in candidate", func(rc *RoutingConfig) {
			p := rc.Profiles[ProfileFixFast]
			p.Candidates = []Candidate{{Runner: types.Runner("gemini"), Model: "g", Effort: types.EffortMedium}}
			rc.Profiles[ProfileFixFast] = p
		}},
		{"undeclared runner in candidate", func(rc *RoutingConfig) {
			delete(rc.Runners, types.RunnerClaude)
		}},
		{"empty candidates", func(rc *RoutingConfig) {
			p := rc.Profiles[ProfileFixFast]
			p.Candidates = nil
			rc.Profiles[ProfileFixFast] = p
		}},
		{"invalid effort", func(rc *RoutingConfig) {
			p := rc.Profiles[ProfileFixFast]
			p.Candidates = append([]Candidate(nil), p.Candidates...)
			p.Candidates[0].Effort = types.Effort("ultra")
			rc.Profiles[ProfileFixFast] = p
		}},
		{"empty model", func(rc *RoutingConfig) {
			p := rc.Profiles[ProfileFixFast]
			p.Candidates = append([]Candidate(nil), p.Candidates...)
			p.Candidates[0].Model = ""
			rc.Profiles[ProfileFixFast] = p
		}},
		{"route references unknown profile", func(rc *RoutingConfig) {
			rc.Routes[types.PurposeInitialReview] = Route{ProfileName("nonexistent")}
		}},
		{"route for unregistered purpose", func(rc *RoutingConfig) {
			rc.Routes[types.Purpose("made_up_purpose")] = Route{ProfileReviewStrong}
		}},
		{"missing route for a purpose", func(rc *RoutingConfig) {
			delete(rc.Routes, types.PurposeInitialReview)
		}},
		{"empty route", func(rc *RoutingConfig) {
			rc.Routes[types.PurposeInitialReview] = Route{}
		}},
		{"non-finite route repeats a profile", func(rc *RoutingConfig) {
			rc.Routes[types.PurposeStructuredFindingRepair] = Route{ProfileFixFast, ProfileFixFast}
		}},
		{"invalid runner failure domain", func(rc *RoutingConfig) {
			rc.Runners[types.RunnerCodex] = RunnerSpec{Executable: "codex", FailureDomain: types.FailureDomain("google")}
		}},
		{"missing runner executable", func(rc *RoutingConfig) {
			rc.Runners[types.RunnerCodex] = RunnerSpec{Executable: "", FailureDomain: types.FailureDomainOpenAI}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := DefaultRoutingConfig()
			tt.mutate(&rc)
			if err := rc.Validate(); err == nil {
				t.Fatalf("%s: Validate() = nil, want error", tt.name)
			}
		})
	}
}

func TestRoutingValidateRejectsNonCanonicalFailureDomain(t *testing.T) {
	rc := DefaultRoutingConfig()
	rc.Runners[types.RunnerCodex] = RunnerSpec{Executable: "codex", FailureDomain: types.FailureDomainAnthropic}
	if err := rc.Validate(); err == nil {
		t.Fatal("expected codex mapped to the anthropic domain to be rejected")
	}
	rc = DefaultRoutingConfig()
	rc.Runners[types.RunnerClaude] = RunnerSpec{Executable: "claude", FailureDomain: types.FailureDomainOpenAI}
	if err := rc.Validate(); err == nil {
		t.Fatal("expected claude mapped to the openai domain to be rejected")
	}
}
