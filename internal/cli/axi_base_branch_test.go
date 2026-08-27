package cli

import (
	"context"
	"strings"
	"testing"
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
