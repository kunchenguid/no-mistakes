package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRStep_TestingSummaryUsesResolvedCompactSetting(t *testing.T) {
	sctx, _ := newEvidencePublishContext(t, "feature/presentation")
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"testing_summary":"Checks passed.","tested":["go test ./internal/config"]}`
	if err := sctx.DB.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", &findings, nil, 100); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, yaml string
		compact    bool
	}{
		{"default", "", true},
		{"expanded", "test:\n  evidence:\n    compact: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, err := config.LoadRepoFromBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			sctx.Config = config.Merge(&config.GlobalConfig{}, repo)
			sctx.Config.Test.Evidence.AttachMedia = false
			_, _, md := (&PRStep{}).buildPipelineSection(sctx, scm.ProviderGitHub)
			if tc.compact {
				if md != "## Testing\n\nChecks passed." {
					t.Fatalf("default output changed:\n%s", md)
				}
			} else if !strings.Contains(md, "- Summary: Checks passed.") || !strings.Contains(md, "- `go test ./internal/config`") {
				t.Fatalf("resolved opt-in not reflected in PR Testing section:\n%s", md)
			}
		})
	}
}
