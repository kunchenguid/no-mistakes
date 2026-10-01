//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestReviewSchemaRetryExhaustionJourney drives the real daemon, gate, and
// Pi adapter through a reviewer whose final JSON keeps failing the review
// schema (issue #1280). Each retry's prompt quotes the previous rejection, so
// the scenario keys every later attempt on that quoted error. The persisted
// Review step error must attribute distinct-field failures, must not claim
// distinct fields for a repeated single field or for non-field failures, must
// name the final rejection only once, and a reviewer that recovers on a retry
// must still complete Review.
func TestReviewSchemaRetryExhaustionJourney(t *testing.T) {
	const valid = `"findings":[],"risk_level":"low","risk_rationale":"r","risk_scope":"source-or-external","reviewed_paths":["%s"]`
	scenario := filepath.Join(t.TempDir(), "review-schema-retry.yaml")
	content := `actions:
  - match: "output missing required field"
    text: "distinct attempt 3"
    structured_raw: '{"findings":[],"tested":"yes","risk_level":"low","risk_rationale":"r","risk_scope":"source-or-external"}'
  - match: "risk_scope must match one of the allowed values"
    text: "distinct attempt 2"
    structured_raw: '{"findings":[],"risk_level":"low","risk_scope":"source-or-external"}'
  - match: "branch: schema-distinct-fields"
    text: "distinct attempt 1"
    structured_raw: '{"findings":[],"risk_level":"low","risk_rationale":"r","risk_scope":"everything"}'
  - match: "findings[3] missing required field"
    text: "same-field attempt 3"
    structured_raw: '{"findings":[{"severity":"info","description":"d","action":"no-op","review_scope":"source"},{"severity":"info","description":"d","action":"no-op","review_scope":"bogus"}],"risk_level":"low","risk_rationale":"r","risk_scope":"source-or-external"}'
  - match: "findings[2].review_scope must match"
    text: "same-field attempt 2"
    structured_raw: '{"findings":[{"severity":"info","description":"d","action":"no-op","review_scope":"source"},{"severity":"info","description":"d","action":"no-op","review_scope":"source"},{"severity":"info","description":"d","action":"no-op","review_scope":"source"},{"severity":"info","description":"d","action":"no-op"}],"risk_level":"low","risk_rationale":"r","risk_scope":"source-or-external"}'
  - match: "branch: schema-same-field"
    text: "same-field attempt 1"
    structured_raw: '{"findings":[{"severity":"info","description":"d","action":"no-op","review_scope":"source"},{"severity":"info","description":"d","action":"no-op","review_scope":"source"},{"severity":"info","description":"d","action":"no-op","review_scope":"bogus"}],"risk_level":"low","risk_rationale":"r","risk_scope":"source-or-external"}'
  - match: "output findings must be array"
    text: "non-field attempt 3"
    structured_raw: '"again just a string"'
  - match: "output must be object"
    text: "non-field attempt 2"
    structured_raw: '{"findings":{},"risk_level":"low","risk_rationale":"r","risk_scope":"source-or-external"}'
  - match: "branch: schema-non-field"
    text: "non-field attempt 1"
    structured_raw: '"just a string"'
  - match: "risk_level must match one of the allowed values"
    text: "recovered on retry"
    structured_raw: '{` + strings.Replace(valid, "%s", "recover.txt", 1) + `}'
  - match: "branch: schema-recovers"
    text: "recover attempt 1"
    structured_raw: '{"findings":[],"risk_level":"extreme","risk_rationale":"r","risk_scope":"source-or-external"}'
`
	if err := os.WriteFile(scenario, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}

	h := NewHarness(t, SetupOpts{Agent: "pi", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	for _, tc := range []struct {
		branch, path string
		want         []string
		forbid       []string
		lastError    string
	}{
		{
			branch: "schema-distinct-fields", path: "distinct.txt",
			want:      []string{"validate review analyzer findings after 3 attempts", "output kept failing validation across distinct fields (risk_scope, risk_rationale, tested)", "attempt 1:", "attempt 2:", "attempt 3:"},
			lastError: "tested must be array",
		},
		{
			branch: "schema-same-field", path: "same.txt",
			want:      []string{"validate review analyzer findings after 3 attempts", "findings[1].review_scope must match one of the allowed values"},
			forbid:    []string{"distinct fields"},
			lastError: "findings[1].review_scope must match one of the allowed values",
		},
		{
			branch: "schema-non-field", path: "nonfield.txt",
			want:      []string{"validate review analyzer findings after 3 attempts", "must be object"},
			forbid:    []string{"distinct fields"},
			lastError: "must be object",
		},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			h.CommitChange(tc.branch, tc.path, "change\n", "exercise review schema retries")
			h.PushToGate(tc.branch)
			run := h.WaitForRun(tc.branch, 120*time.Second)
			step, ok := findStep(run.Steps, types.StepReview)
			if !ok {
				t.Fatalf("missing review step")
			}
			msg := deref(step.Error)
			t.Logf("persisted run: branch=%s status=%s review_status=%s\nreview error: %s", run.Branch, run.Status, step.Status, msg)
			if run.Status != types.RunFailed || step.Status != types.StepStatusFailed {
				t.Fatalf("run=%s review=%s, want both failed", run.Status, step.Status)
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("review error missing %q", w)
				}
			}
			for _, f := range tc.forbid {
				if strings.Contains(msg, f) {
					t.Errorf("review error must not contain %q", f)
				}
			}
			if got := strings.Count(msg, tc.lastError); tc.forbid == nil && got != 1 {
				t.Errorf("final rejection %q appears %d times, want once", tc.lastError, got)
			}
			for _, later := range []types.StepName{types.StepTest, types.StepDocument, types.StepLint} {
				if s, ok := findStep(run.Steps, later); ok && s.Status != types.StepStatusPending && s.Status != types.StepStatusSkipped {
					t.Errorf("%s ran after a failed review: %s", later, s.Status)
				}
			}
			reviews := 0
			for _, inv := range h.AgentInvocations() {
				if strings.Contains(inv.Prompt, "branch: "+tc.branch) && strings.Contains(inv.Prompt, "Review the code changes and return structured findings") {
					reviews++
				}
			}
			if reviews != 3 {
				t.Errorf("review agent invocations for %s = %d, want 3", tc.branch, reviews)
			}
		})
	}

	t.Run("schema-recovers", func(t *testing.T) {
		h.CommitChange("schema-recovers", "recover.txt", "change\n", "exercise review schema recovery")
		h.PushToGate("schema-recovers")
		deadline := time.Now().Add(120 * time.Second)
		for {
			run := h.ActiveRun("schema-recovers")
			if run == nil {
				for _, r := range h.Runs() {
					if r.Branch == "schema-recovers" {
						run = &r
					}
				}
			}
			if run != nil {
				if step, ok := findStep(run.Steps, types.StepReview); ok && step.Status != types.StepStatusPending && step.Status != types.StepStatusRunning {
					t.Logf("persisted run: branch=%s status=%s review_status=%s review_error=%q", run.Branch, run.Status, step.Status, deref(step.Error))
					if step.Status != types.StepStatusCompleted {
						t.Fatalf("review status = %s, want completed after a valid retry", step.Status)
					}
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("review never settled")
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
}
