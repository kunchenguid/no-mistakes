package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
)

func TestHumanSyncSummaryPinnedHeadKeepLocalDoesNotClaimAnArchive(t *testing.T) {
	t.Parallel()

	got := humanSyncSummary(branchsync.State{
		State:    branchsync.StatePipelineOwned,
		Safety:   "blocked_pipeline_owned_recoverable",
		Recovery: &branchsync.RecoveryEvidence{Source: "pinned_head", KeepLocal: true, Proof: "verified"},
	})
	if strings.Contains(got, "archive") {
		t.Fatalf("pinned-head summary mentions an archive: %q", got)
	}
	if !strings.Contains(got, "are not on the branch") || !strings.Contains(got, "--recover --keep-local") {
		t.Fatalf("pinned-head summary = %q", got)
	}
}
