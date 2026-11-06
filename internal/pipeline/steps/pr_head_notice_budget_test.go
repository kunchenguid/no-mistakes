package steps

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestProviderPlugin_PRStepRetainsHeadProofNoticeAtTinyBodyCaps(t *testing.T) {
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed, config.PRAppendixMinimal} {
		for _, limit := range []int{600, 80} {
			t.Run(mode+"/"+strconv.Itoa(limit), func(t *testing.T) {
				t.Parallel()
				dir, base, head := setupGitRepo(t)
				sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
				statePath, _ := fakeProviderPlugin(t, sctx, fakeplugin.State{MaxPRBodyChars: limit})
				sctx.Config.PR.Appendix = mode
				for _, name := range []types.StepName{
					types.StepIntent, types.StepRebase, types.StepReview, types.StepTest,
					types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI,
				} {
					step, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
					if err != nil {
						t.Fatal(err)
					}
					if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
						t.Fatal(err)
					}
					findings := `{"findings":[],"summary":"complete"}`
					if name == types.StepReview {
						findings = `{"findings":[],"risk_level":"low","risk_rationale":"fixture"}`
					}
					if name == types.StepTest {
						findings = liveValidatedFindingsJSON(t, []types.TestScenario{{Name: "publication", Result: types.ScenarioResultPass, Live: true, Evidence: "recorded fixture"}}, types.TestVerdictGo, head)
					}
					if err := sctx.DB.SetStepFindings(step.ID, findings); err != nil {
						t.Fatal(err)
					}
				}
				out, executeErr := (&PRStep{}).Execute(sctx)
				state, err := fakeplugin.Load(statePath)
				if err != nil {
					t.Fatal(err)
				}
				if limit == 80 {
					if executeErr == nil || out != nil || len(state.PRs) != 0 {
						t.Fatalf("unfittable mandatory metadata published: outcome=%+v err=%v PRs=%+v", out, executeErr, state.PRs)
					}
					return
				}
				if executeErr != nil || out == nil || len(state.PRs) != 1 {
					t.Fatalf("capped publication failed: outcome=%+v err=%v PRs=%+v", out, executeErr, state.PRs)
				}
				body := state.PRs[0].Body
				if scm.PRBodyLen(body) > limit {
					t.Fatalf("published body exceeds cap %d: %s", limit, body)
				}
				const notice = "PR head proof unavailable for provider plugin:ssm; the exact pushed head was not verified."
				if !strings.Contains(body, notice) {
					t.Fatalf("capped publication dropped head-proof disclosure: %s", body)
				}
				marker := extractPipelineAttestationMarker(body)
				payload := strings.TrimSuffix(strings.TrimPrefix(marker, pipelineAttestationCommentPrefix), pipelineAttestationCommentClosingToken)
				var attestation struct {
					HeadSHA        string                          `json:"head_sha"`
					Steps          []struct{ Step, Status string } `json:"steps"`
					LiveValidation *struct {
						Verdict string `json:"verdict"`
						Live    int    `json:"live"`
						Total   int    `json:"total"`
					} `json:"live_validation"`
				}
				if err := json.Unmarshal([]byte(payload), &attestation); err != nil {
					t.Fatalf("invalid retained attestation: %v; body=%s", err, body)
				}
				if attestation.HeadSHA != head || len(attestation.Steps) != 9 || attestation.LiveValidation == nil || attestation.LiveValidation.Verdict != types.TestVerdictGo || attestation.LiveValidation.Live != 1 || attestation.LiveValidation.Total != 1 {
					t.Fatalf("capped publication lost complete-run metadata: %+v", attestation)
				}
			})
		}
	}
}
