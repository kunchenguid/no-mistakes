package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestFormatPushBranchPushOption(t *testing.T) {
	opt := formatPushBranchPushOption("stacked-pr")
	if opt != "no-mistakes.push-branch=stacked-pr" {
		t.Fatalf("formatPushBranchPushOption = %q", opt)
	}
	if got := formatPushBranchPushOption("   "); got != "" {
		t.Fatalf("blank push branch = %q, want empty", got)
	}
}

func TestParsePushBranchPushOptions(t *testing.T) {
	got, err := parsePushBranchPushOptions([]string{
		"no-mistakes.push-branch=first",
		"no-mistakes.skip=review",
		"no-mistakes.push-branch=stacked-pr",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "stacked-pr" {
		t.Fatalf("parsePushBranchPushOptions = %q, want last value stacked-pr", got)
	}
	if _, err := parsePushBranchPushOptions([]string{"no-mistakes.push-branch=  "}); err == nil {
		t.Fatal("empty push-branch push option must fail closed")
	}
}

func TestConflictingActiveRunPushBranch_AllowsMatchingOrOmitted(t *testing.T) {
	t.Parallel()
	stored := "stacked-pr"
	run := &ipc.RunInfo{ID: "run-1", PushBranch: &stored}
	if err := conflictingActiveRunPushBranch(run, "stacked-pr"); err != nil {
		t.Fatalf("matching --push-branch should reattach: %v", err)
	}
	if err := conflictingActiveRunPushBranch(run, ""); err != nil {
		t.Fatalf("omitting --push-branch should reattach: %v", err)
	}
	if err := conflictingActiveRunPushBranch(nil, "stacked-pr"); err != nil {
		t.Fatalf("no active run means no conflict: %v", err)
	}
}

func TestConflictingActiveRunPushBranch_RefusesMismatch(t *testing.T) {
	t.Parallel()
	stored := "other-pr"
	run := &ipc.RunInfo{ID: "run-1", PushBranch: &stored}
	err := conflictingActiveRunPushBranch(run, "stacked-pr")
	if err == nil {
		t.Fatal("expected conflict when --push-branch differs from the active run")
	}
	if !strings.Contains(err.Error(), "run-1") || !strings.Contains(err.Error(), "other-pr") {
		t.Fatalf("error = %v, want it to name the run and stored publish branch", err)
	}
}

func TestConflictingActiveRunPushBranch_RefusesWhenActiveRunHasNoBinding(t *testing.T) {
	t.Parallel()
	run := &ipc.RunInfo{ID: "run-1"}
	err := conflictingActiveRunPushBranch(run, "stacked-pr")
	if err == nil {
		t.Fatal("expected conflict when reattaching would discard --push-branch")
	}
	if !strings.Contains(err.Error(), "run-1") {
		t.Fatalf("error = %v, want it to name the active run", err)
	}
}

func TestRerunParams_CarriesPushBranch(t *testing.T) {
	t.Parallel()
	params := rerunParams("repo-1", "feature/x", []types.StepName{types.StepReview}, "user goal", "develop", "stacked-pr")
	if params.PushBranch != "stacked-pr" {
		t.Fatalf("rerunParams.PushBranch = %q, want stacked-pr", params.PushBranch)
	}
	if params.PRBaseBranch != "develop" {
		t.Fatalf("rerunParams.PRBaseBranch = %q, want develop", params.PRBaseBranch)
	}
}

// TestAxiRunPushBranchRefusesOlderDaemon pins the probe contract: an older
// daemon decodes push_branch permissively and would run unbound, publishing
// onto the local branch, so a flagged run must be refused before launch.
func TestAxiRunPushBranchRefusesOlderDaemon(t *testing.T) {
	probes := []struct {
		name  string
		probe func() (interface{}, error)
	}{
		{name: "probe method unknown", probe: nil},
		{name: "probe declined", probe: func() (interface{}, error) { return &ipc.ProbePushBranchResult{OK: false}, nil }},
		{name: "probe undecodable", probe: func() (interface{}, error) { return json.RawMessage(`"yes"`), nil }},
	}
	for _, tc := range probes {
		t.Run(tc.name, func(t *testing.T) {
			launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil }, func(srv *ipc.Server) {
				if tc.probe != nil {
					srv.Handle(ipc.MethodProbePushBranch, func(context.Context, json.RawMessage) (interface{}, error) { return tc.probe() })
				}
			})
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&out)
			err := runAxiRunWithLaunchProof(cmd, false, nil, "rebind the PR", "", "stacked-pr", false, "", "", defaultAxiWait)
			if err == nil {
				t.Fatalf("axi run --push-branch should refuse an older daemon:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "too old to honor --push-branch") {
				t.Fatalf("output should name the daemon capability, got:\n%s", out.String())
			}
			if len(*launched) != 0 {
				t.Fatalf("run was started on a daemon that would drop the binding: %v", *launched)
			}
		})
	}
}

// TestRerunPushBranchRefusesOlderDaemon mirrors the run path: the re-bind
// rides MethodRerun's PushBranch field, which an older daemon drops.
func TestRerunPushBranchRefusesOlderDaemon(t *testing.T) {
	launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil })
	var out bytes.Buffer
	cmd := newRerunCmd()
	cmd.SetArgs([]string{"--push-branch", "stacked-pr"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("rerun --push-branch should refuse an older daemon:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "too old to honor --push-branch") {
		t.Fatalf("error should name the daemon capability, got: %v", err)
	}
	if len(*launched) != 0 {
		t.Fatalf("rerun was started on a daemon that would drop the binding: %v", *launched)
	}
}

// TestAxiRunPushBranchPassesCapableDaemonProbe: a daemon answering OK is
// trusted with the binding and the refusal text never appears.
func TestAxiRunPushBranchPassesCapableDaemonProbe(t *testing.T) {
	olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil }, func(srv *ipc.Server) {
		srv.Handle(ipc.MethodProbePushBranch, func(context.Context, json.RawMessage) (interface{}, error) {
			return &ipc.ProbePushBranchResult{OK: true}, nil
		})
	})
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	_ = runAxiRunWithLaunchProof(cmd, false, nil, "rebind the PR", "", "stacked-pr", false, "", "", defaultAxiWait)
	if strings.Contains(out.String(), "too old to honor --push-branch") {
		t.Fatalf("capable daemon was refused:\n%s", out.String())
	}
}
