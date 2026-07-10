package types

import "testing"

func TestRunnerValidate(t *testing.T) {
	for _, r := range []Runner{RunnerCodex, RunnerClaude} {
		if err := r.Validate(); err != nil {
			t.Fatalf("Runner(%q).Validate() = %v, want nil", r, err)
		}
	}
	for _, bad := range []Runner{"", "gpt", "openai", "Codex"} {
		if err := Runner(bad).Validate(); err == nil {
			t.Fatalf("Runner(%q).Validate() = nil, want error", bad)
		}
	}
}

func TestRunnerFailureDomain(t *testing.T) {
	if d, err := RunnerCodex.FailureDomain(); err != nil || d != FailureDomainOpenAI {
		t.Fatalf("RunnerCodex.FailureDomain() = %q, %v; want %q", d, err, FailureDomainOpenAI)
	}
	if d, err := RunnerClaude.FailureDomain(); err != nil || d != FailureDomainAnthropic {
		t.Fatalf("RunnerClaude.FailureDomain() = %q, %v; want %q", d, err, FailureDomainAnthropic)
	}
	if _, err := Runner("x").FailureDomain(); err == nil {
		t.Fatal("Runner(x).FailureDomain() = nil error, want error")
	}
}

func TestEffortValidate(t *testing.T) {
	for _, e := range []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh} {
		if err := e.Validate(); err != nil {
			t.Fatalf("Effort(%q).Validate() = %v, want nil", e, err)
		}
	}
	for _, bad := range []Effort{"", "minimal", "ultra", "Medium", "HIGH"} {
		if err := Effort(bad).Validate(); err == nil {
			t.Fatalf("Effort(%q).Validate() = nil, want error", bad)
		}
	}
}
