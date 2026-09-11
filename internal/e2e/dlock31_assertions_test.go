//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func assertDLOCK31Retained(t *testing.T, h *Harness, database *db.DB, binding *db.RetainedCIRepair, observed *ipc.RunInfo) {
	t.Helper()
	stored, err := database.GetRun(observed.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := database.RetainedCIRepair(observed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if actual == nil || *actual != *binding {
		t.Fatalf("retained binding changed: got %+v want %+v", actual, binding)
	}
	step, ok := findStep(observed.Steps, types.StepCI)
	if !ok || step.ID != binding.StepID {
		t.Fatalf("CI step identity changed: %+v", step)
	}
	if stored.HeadSHA != binding.RecordedHead || h.UpstreamBranchSHA(binding.Branch) != binding.RecordedHead {
		t.Fatal("park/recovery unexpectedly published or rewrote recorded head")
	}
	if stored.ReviewApprovedHeadSHA == nil || *stored.ReviewApprovedHeadSHA != binding.ReviewedHead {
		t.Fatal("review authority changed")
	}
	assertDLOCK31Git(t, h, stored.WorktreePath(), binding)
	t.Logf("parked evidence unchanged: run=%s step=%s branch=%s recorded=%s reviewed=%s retained=%s", binding.RunID, binding.StepID, binding.Branch, binding.RecordedHead, binding.ReviewedHead, binding.RetainedHead)
}

func assertDLOCK31Git(t *testing.T, h *Harness, workdir string, binding *db.RetainedCIRepair) {
	t.Helper()
	if workdir == "" {
		t.Fatal("run has no recorded worktree")
	}
	for _, ref := range []string{"HEAD", "refs/no-mistakes/ci-repair/" + binding.RunID} {
		out, err := h.runGit(t.Context(), workdir, "rev-parse", ref)
		if err != nil || strings.TrimSpace(string(out)) != binding.RetainedHead {
			t.Fatalf("%s does not preserve retained head: %s %v", ref, out, err)
		}
	}
	if out, err := h.runGit(t.Context(), workdir, "status", "--porcelain"); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("retained worktree not clean: %s %v", out, err)
	}
	if out, err := h.runGit(t.Context(), workdir, "show", "HEAD:repair.txt"); err != nil || string(out) != "repaired\n" {
		t.Fatalf("wrong retained content: %s %v", out, err)
	}
	if out, err := h.runGit(t.Context(), workdir, "rev-list", "--count", binding.RecordedHead+"..HEAD"); err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Fatalf("expected exactly one preserved repair commit: %s %v", out, err)
	}
}
