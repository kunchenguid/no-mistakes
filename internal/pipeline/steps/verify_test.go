package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestVerifyStep_SkipsWhenUnchanged proves Verify skips fresh verification when
// the sealed candidate exactly matches the latest strong-reviewed candidate.
func TestVerifyStep_SkipsWhenUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Fatal("verify must not invoke the agent when the candidate is unchanged")
			return nil, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, "base", "head", config.Commands{})
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, "same-sha", "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, "same-sha", "pre_verify"); err != nil {
		t.Fatal(err)
	}

	step := &VerifyStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected skipped verification to not gate")
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no agent calls on skip, got %d", len(ag.calls))
	}
}

// TestVerifyStep_FreshVerificationPasses proves a changed candidate triggers a
// fresh aggregate verification that passes on empty findings.
func TestVerifyStep_FreshVerificationPasses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"candidate verified"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	// Only a pre-verify seal (no matching reviewed seal) forces fresh verification.
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, headSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}

	step := &VerifyStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected clean fresh verification to pass")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 aggregate verification call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "final aggregate verification") {
		t.Fatalf("expected the aggregate verification prompt, got: %s", ag.calls[0].Prompt)
	}
}

// TestVerifyStep_GatesOnBlockingFindings proves a blocking verification finding
// gates the pipeline before Push.
func TestVerifyStep_GatesOnBlockingFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","file":"main.go","description":"regression introduced after review"}],"risk_level":"high","risk_rationale":"unreviewed regression"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, headSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}

	step := &VerifyStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a blocking verification finding to gate")
	}
}

// TestVerifyStep_FailsClosedWithoutSeal proves Verify fails closed when nothing
// has been sealed.
func TestVerifyStep_FailsClosedWithoutSeal(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	step := &VerifyStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected verify to fail closed with no sealed candidate")
	}
}

// TestVerifyPurpose_Escalates proves normal verification uses review_strong while
// intent-sensitive and post-repair verification escalate to authority_strong.
func TestVerifyPurpose_Escalates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, "base", "head", config.Commands{})

	if got := verifyPurpose(sctx); got != types.PurposeNormalAggregateVerification {
		t.Fatalf("normal verify purpose = %q, want normal aggregate verification", got)
	}

	sctx.UserIntent = "ship the extracted intent safely"
	if got := verifyPurpose(sctx); got != types.PurposeEscalatedAggregateVerification {
		t.Fatalf("intent-sensitive verify purpose = %q, want escalated aggregate verification", got)
	}

	sctx.UserIntent = ""
	sctx.Fixing = true
	if got := verifyPurpose(sctx); got != types.PurposeEscalatedAggregateVerification {
		t.Fatalf("post-repair verify purpose = %q, want escalated aggregate verification", got)
	}
}
