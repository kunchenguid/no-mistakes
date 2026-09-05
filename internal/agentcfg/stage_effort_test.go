package agentcfg

import (
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func TestStageEffortDispatch(t *testing.T) {
	stages := StageEfforts{"intent": "low", "rebase": "low", "review": "high", "review-fix": "medium", "test": "low", "document": "low", "lint": "low", "pr": "low", "ci": "low"}
	if err := ValidateStageEfforts(types.AgentPi, stages); err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []string{"intent", "rebase", "review", "review-fix", "test-evidence", "test-fix", "document", "document-fix", "housekeeping", "lint", "lint-fix", "pr", "ci", "unrelated"} {
		want := EffortLow
		switch purpose {
		case "review":
			want = EffortHigh
		case "review-fix":
			want = EffortMedium
		case "unrelated":
			want = EffortMax
		}
		got := StageProfile(Profile{Model: "same", Effort: EffortMax}, stages, purpose)
		if got.Effort != want || got.Model != "same" {
			t.Errorf("%s = %+v", purpose, got)
		}
	}
	delete(stages, "review-fix")
	if got := StageProfile(Profile{Effort: EffortLow}, stages, "review-fix"); got.Effort != EffortLow {
		t.Fatal("fix inherited review instead of global effort")
	}
}

func TestSameStageEffortHarnessMappings(t *testing.T) {
	for _, tc := range []struct {
		name types.AgentName
		raw  []string
	}{
		{types.AgentClaude, []string{"--effort=medium"}},
		{types.AgentPi, []string{"--thinking=medium"}},
		{types.AgentCodex, []string{"--config", `model_reasoning_effort="medium"`}},
		{types.AgentGrok, []string{"--reasoning-effort", "medium"}},
		{types.AgentCopilot, []string{"--effort", "medium"}},
	} {
		stages := StageEfforts{"document": "high", "lint": "low"}
		if !SameStageEffort(tc.name, Profile{}, stages, tc.raw, "document", "lint") {
			t.Errorf("%s raw precedence lost", tc.name)
		}
		if SameStageEffort(tc.name, Profile{}, stages, []string{"--model", "medium"}, "document", "lint") {
			t.Errorf("%s model mistaken for effort", tc.name)
		}
	}
	if SameStageEffort(types.AgentOpenCode, Profile{}, StageEfforts{"document": "high", "lint": "low"}, nil, "document", "lint") {
		t.Fatal("request-level effort ignored")
	}
}
