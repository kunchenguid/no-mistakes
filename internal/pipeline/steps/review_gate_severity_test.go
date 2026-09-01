package steps

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// review.gate_severity decides whether a warning-only review parks: the
// default keeps the pre-policy rule (error or warning parks); "error" reports
// warnings without parking.
func TestReviewStep_GateSeverityDecidesWhetherWarningsPark(t *testing.T) {
	warningOnly := `{"findings":[{"file":"a.txt","line":1,"severity":"warning","action":"ask-user","description":"tidy"}],"risk_level":"low","risk_rationale":"cosmetic","risk_scope":"source-or-external"}`
	withError := `{"findings":[{"file":"a.txt","line":1,"severity":"error","action":"ask-user","description":"broken"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`

	run := func(t *testing.T, severity, findings string) bool {
		t.Helper()
		dir, baseSHA, headSHA := setupGitRepo(t)
		gitCmd(t, dir, "checkout", "--detach", headSHA)
		ag := &mockAgent{
			name: "gate-severity",
			runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(findings)}, nil
			},
		}
		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
		sctx.Config.Review.GateSeverity = severity
		outcome, err := (&ReviewStep{}).Execute(sctx)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		return outcome.NeedsApproval
	}

	if !run(t, "", warningOnly) {
		t.Fatalf("default gate severity: a warning finding must need approval")
	}
	if !run(t, config.ReviewGateSeverityWarning, warningOnly) {
		t.Fatalf("gate_severity: warning: a warning finding must need approval")
	}
	if run(t, config.ReviewGateSeverityError, warningOnly) {
		t.Fatalf("gate_severity: error: a warning-only review must not need approval")
	}
	if !run(t, config.ReviewGateSeverityError, withError) {
		t.Fatalf("gate_severity: error: an error finding must still need approval")
	}
}
