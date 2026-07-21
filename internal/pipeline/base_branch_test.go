package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestStepContextBaseBranchUsesFrozenRunSnapshot(t *testing.T) {
	sctx := &StepContext{
		Run:  &db.Run{BaseBranch: "staging"},
		Repo: &db.Repo{DefaultBranch: "main", BaseBranch: "release/v2"},
	}
	if got := sctx.BaseBranch(); got != "staging" {
		t.Fatalf("BaseBranch = %q, want frozen staging", got)
	}
}
