package cli

import (
	"strings"
	"testing"
)

func TestInitBaseBranchFlags(t *testing.T) {
	cmd := newInitCmd()
	if cmd.Flags().Lookup("base-branch") == nil {
		t.Fatal("--base-branch flag is not registered")
	}
	if cmd.Flags().Lookup("clear-base-branch") == nil {
		t.Fatal("--clear-base-branch flag is not registered")
	}
}

func TestInitRejectsConflictingBaseBranchFlagsBeforeOpeningState(t *testing.T) {
	cmd := newInitCmd()
	cmd.SetArgs([]string{"--base-branch", "staging", "--clear-base-branch"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v, want mutually exclusive base-branch error", err)
	}
}
