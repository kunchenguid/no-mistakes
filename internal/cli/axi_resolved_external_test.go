package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestAxiRespond_ResolvedExternalForwardsReasonAndFindingWithoutCoverageBinding(t *testing.T) {
	testResolvedExternalCLI(t, executeCmd)
}

func TestNoMistakesBinary_ResolvedExternal(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	testResolvedExternalCLI(t, func(args ...string) (string, error) {
		return execNoMistakesErr(t, bin, args...)
	})
}

func testResolvedExternalCLI(t *testing.T, invoke func(...string) (string, error)) {
	t.Helper()
	var got map[string]any
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		if err := json.Unmarshal(raw, &got); err != nil {
			return nil, err
		}
		return &ipc.RespondResult{OK: true, ResolvedExternal: []string{"grant"}}, nil
	}})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if got != nil {
			return fx.completed(), nil
		}
		run := fx.awaiting()
		if err := json.Unmarshal([]byte(`{"id":"review-step","step_name":"review","latest_round_id":"parked-round","findings_json":"{\"findings\":[{\"id\":\"grant\"}]}"}`), &run.Steps[0]); err != nil {
			return nil, err
		}
		return run, nil
	})
	if out, err := invoke("axi", "respond", "--step", "review", "--action", "resolved_external", "--findings", "grant", "--wait", "3s"); err == nil || !strings.Contains(out, "requires a Review gate") || got != nil {
		t.Fatalf("CLI accepted an external disposition without a reason: %v %s", err, out)
	}
	out, err := invoke("axi", "respond", "--step", "review", "--action", "resolved_external", "--findings", "grant", "--reason", "verified on machine", "--wait", "3s")
	if err != nil {
		t.Fatalf("external response failed: %v\n%s", err, out)
	}
	if got["action"] != "resolved_external" || got["approval_reason"] != "verified on machine" || got["coverage"] != nil || !strings.Contains(out, "resolved_external") {
		t.Fatalf("external disposition missing from request or echo: %+v\n%s", got, out)
	}
	ids, _ := got["finding_ids"].([]any)
	if len(ids) != 1 || ids[0] != "grant" {
		t.Fatalf("finding IDs lost: %+v", got)
	}
}
