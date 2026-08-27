package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestFormatPRBaseBranchPushOption(t *testing.T) {
	opt := formatPRBaseBranchPushOption("epic/feature")
	if opt != "no-mistakes.pr-base-branch=epic/feature" {
		t.Fatalf("formatPRBaseBranchPushOption = %q", opt)
	}
	if got := formatPRBaseBranchPushOption("   "); got != "" {
		t.Fatalf("blank base branch = %q, want empty", got)
	}
}

func TestParsePRBaseBranchPushOptions(t *testing.T) {
	got, err := parsePRBaseBranchPushOptions([]string{
		"no-mistakes.pr-base-branch=develop",
		"no-mistakes.pr-base-branch=epic/feature",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "epic/feature" {
		t.Fatalf("parsePRBaseBranchPushOptions = %q, want last value epic/feature", got)
	}
}

func TestValidateAxiRunBaseBranch_RejectsInvalidName(t *testing.T) {
	t.Parallel()
	err := validateAxiRunBaseBranch(context.Background(), "bad..branch")
	if err == nil {
		t.Fatal("expected error for invalid branch name")
	}
	if !strings.Contains(err.Error(), "--base-branch") {
		t.Fatalf("error = %v, want it to name --base-branch", err)
	}
}

func TestValidateAxiRunBaseBranch_AllowsEmpty(t *testing.T) {
	t.Parallel()
	if err := validateAxiRunBaseBranch(context.Background(), ""); err != nil {
		t.Fatalf("empty base branch should be allowed: %v", err)
	}
}

func TestConflictingActiveRunPRBaseBranch_AllowsMatchingOrOmitted(t *testing.T) {
	t.Parallel()
	stored := "epic/feature"
	run := &ipc.RunInfo{ID: "run-1", PRBaseBranch: &stored}
	if err := conflictingActiveRunPRBaseBranch(run, "epic/feature"); err != nil {
		t.Fatalf("matching --base-branch should reattach: %v", err)
	}
	if err := conflictingActiveRunPRBaseBranch(run, ""); err != nil {
		t.Fatalf("omitting --base-branch should reattach: %v", err)
	}
}

func TestConflictingActiveRunPRBaseBranch_RefusesMismatch(t *testing.T) {
	t.Parallel()
	stored := "develop"
	run := &ipc.RunInfo{ID: "run-1", PRBaseBranch: &stored}
	err := conflictingActiveRunPRBaseBranch(run, "epic/feature")
	if err == nil {
		t.Fatal("expected conflict when --base-branch differs from the active run")
	}
	if !strings.Contains(err.Error(), "run-1") || !strings.Contains(err.Error(), "develop") {
		t.Fatalf("error = %v, want it to name the run and stored base", err)
	}
}

func TestConflictingActiveRunPRBaseBranch_RefusesWhenActiveRunHasNoOverride(t *testing.T) {
	t.Parallel()
	run := &ipc.RunInfo{ID: "run-1"}
	err := conflictingActiveRunPRBaseBranch(run, "epic/feature")
	if err == nil {
		t.Fatal("expected conflict when reattaching would discard --base-branch")
	}
	if !strings.Contains(err.Error(), "run-1") {
		t.Fatalf("error = %v, want it to name the active run", err)
	}
}
