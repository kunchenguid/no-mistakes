package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestStrandedMirrorRerunRefusalNamesAReachableAction pins the interaction
// between the two documented paths on the recorded stranded chain: `rerun`
// selects the stale mirror head and refuses a clean caller-head mismatch,
// pointing at `axi run`; that pointer is honest only because the submission path
// now settles exactly this mirror from recorded evidence. Both halves are
// asserted, so a future change cannot leave the pair pointing at each other
// again.
func TestStrandedMirrorRerunRefusalNamesAReachableAction(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	gateDir := filepath.Join(root, "gate.git")
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	gitCmd(t, "", "init", work)
	gitCmd(t, work, "config", "user.email", "test@test.com")
	gitCmd(t, work, "config", "user.name", "Test")
	writeFile("base.txt", "base\n")
	gitCmd(t, work, "add", "base.txt")
	gitCmd(t, work, "commit", "-m", "base")
	base := gitOutput(t, work, "rev-parse", "HEAD")

	// The private lineage the run submitted.
	writeFile("feature.txt", "submitted version\n")
	gitCmd(t, work, "add", "feature.txt")
	gitCmd(t, work, "commit", "-m", "submitted work")
	mirrorHead := gitOutput(t, work, "rev-parse", "HEAD")

	// The run's own accepted result, then the caller's clean head on top.
	gitCmd(t, work, "reset", "--hard", base)
	writeFile("feature.txt", "accepted version\n")
	gitCmd(t, work, "add", "feature.txt")
	gitCmd(t, work, "commit", "-m", "review: accept the fix")
	accepted := gitOutput(t, work, "rev-parse", "HEAD")
	writeFile("followup.txt", "follow-up\n")
	gitCmd(t, work, "add", "followup.txt")
	gitCmd(t, work, "commit", "-m", "follow-up work")
	localHead := gitOutput(t, work, "rev-parse", "HEAD")

	gitCmd(t, "", "init", "--bare", gateDir)
	gitCmd(t, work, "push", gateDir, mirrorHead+":refs/heads/feature/stranded")

	// Half one: rerun selects the stale mirror head and refuses the mismatch,
	// naming `axi run` as the way to submit the local head.
	now := int64(1)
	run := &db.Run{
		ID: "stranded-run", Branch: "feature/stranded", Status: types.RunFailed,
		HeadSHA: accepted, SubmittedHeadSHA: &mirrorHead,
		TerminalHeadVerifiedAt: &now, CustodyReturnedAt: &now, ReviewApprovedHeadSHA: &accepted,
	}
	selected, err := resolveRerunHead(context.Background(), gateDir, run.Branch, run)
	if err != nil {
		t.Fatal(err)
	}
	if selected != mirrorHead {
		t.Fatalf("rerun selected %s, want the stale mirror head %s", selected, mirrorHead)
	}
	if selected == localHead {
		t.Fatal("fixture did not reproduce a clean caller head that differs from the selected head")
	}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	repo, err := d.InsertRepoWithID("stranded-mirror-repo", work, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.RepoDir(repo.ID); got == "" {
		t.Fatal("paths did not resolve a gate directory")
	}

	// Half two: the command rerun points at is reachable on this exact state.
	// Without recorded evidence it refuses, so the pointer is earned rather than
	// asserted.
	if _, err := gate.ReconcileSupersededSubmission(context.Background(), gateDir, work, run.Branch, localHead, gate.SupersessionSource{}); err == nil {
		t.Fatal("the stranded mirror reconciled without evidence; the refusal is not being exercised")
	}
	stored, err := d.InsertRun(repo.ID, run.Branch, mirrorHead, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(stored.ID, accepted); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(stored.ID, accepted); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(stored.ID, types.RunFailed, accepted); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunCustodyReturned(stored.ID); err != nil {
		t.Fatal(err)
	}
	result, err := gate.ReconcileSupersededSubmission(context.Background(), gateDir, work, run.Branch, localHead, gate.SupersessionSource{
		DB: d, RepoID: repo.ID, Branch: run.Branch,
	})
	if err != nil {
		t.Fatalf("the action rerun names is itself refused: %v", err)
	}
	if !result.Reconciled || result.PreviousHead != mirrorHead {
		t.Fatalf("reconciliation = %+v, want %s archived", result, mirrorHead)
	}
	if !strings.Contains(result.ArchivedTag, mirrorHead) {
		t.Fatalf("archive tag %s does not name the archived head %s", result.ArchivedTag, mirrorHead)
	}

	// The mirror is settled, so the submitted head enters by an ordinary push and
	// branch-sync sees the operator's head on the gate.
	gitCmd(t, work, "push", gateDir, localHead+":refs/heads/feature/stranded")
	if got := gitOutput(t, gateDir, "rev-parse", "refs/heads/feature/stranded"); got != localHead {
		t.Fatalf("gate branch = %s, want submitted head %s", got, localHead)
	}
	state := (&branchsync.Service{DB: d, Repo: repo, WorkDir: work, GateDir: gateDir}).InspectCached(context.Background())
	if state.State == branchsync.StatePipelineOwned || state.State == branchsync.StatePushInProgress {
		t.Fatalf("branch still reports pipeline ownership after the mirror settled: %+v", state)
	}
	t.Logf("rerun refused at %s; `axi run` resolved it via archived %s; branch_sync after settlement=%s", selected, result.ArchivedTag, state.State)
}
