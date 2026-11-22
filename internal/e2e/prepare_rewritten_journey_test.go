//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// rewrittenJourney is the end-user shape `axi sync --prepare-rewritten` exists
// for: a real run validated and published a head, then the operator
// deliberately rebased that branch onto an advanced main without publishing
// the replacement.
type rewrittenJourney struct {
	h         *Harness
	operator  string
	gateDir   string
	branch    string
	runID     string
	published string
	rebased   string
}

func newRewrittenJourney(t *testing.T, branch string) rewrittenJourney {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	h.CommitChange("init-"+strings.ReplaceAll(branch, "/", "-"), "seed.txt", "seed\n", "seed init")
	initWorktree := h.AddWorktree("init-" + strings.ReplaceAll(branch, "/", "-"))
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	h.CommitChange(branch, "feature.txt", "feature\n", "add feature")
	operator := h.AddWorktree(branch)
	out, err := h.RunInDir(operator, "axi", "run", "--yes", "--intent", "publish the feature")
	if err != nil || !strings.Contains(out, "outcome: passed") {
		t.Fatalf("first validated run: %v\n%s", err, out)
	}
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("first run status = %s", run.Status)
	}
	published := h.UpstreamBranchSHA(branch)
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if got := gitOut(t, h, gateDir, "rev-parse", "refs/heads/"+branch); got != published {
		t.Fatalf("gate lane %s != published %s", got, published)
	}
	if got := gitOut(t, h, operator, "rev-parse", "HEAD"); got != published {
		if out, err := h.RunInDir(operator, "axi", "sync"); err != nil {
			t.Fatalf("sync to published head: %v\n%s", err, out)
		}
	}

	h.CommitChange("main", "upstream-advance.txt", "advance\n", "upstream advance")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("advance upstream main: %v\n%s", err, out)
	}
	gitOut(t, h, operator, "rebase", "main")
	// The rewrite deliberately reworks the published content, so the old
	// gate lane holds content the new head does not: exactly the case
	// private-mirror reconciliation refuses without a preservation record.
	if err := os.WriteFile(filepath.Join(operator, "feature.txt"), []byte("feature reworked on new base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, h, operator, "commit", "-a", "--amend", "--no-edit")
	rebased := gitOut(t, h, operator, "rev-parse", "HEAD")
	if rebased == published {
		t.Fatal("rebase did not rewrite the published head")
	}
	return rewrittenJourney{h: h, operator: operator, gateDir: gateDir, branch: branch, runID: run.ID, published: published, rebased: rebased}
}

func gitOut(t *testing.T, h *Harness, dir string, args ...string) string {
	t.Helper()
	out, err := h.runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func saveRewriteEvidence(t *testing.T, name, content string) {
	t.Helper()
	dir := os.Getenv("NM_LIVE_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}

func (j rewrittenJourney) assertNothingMoved(t *testing.T, label string) {
	t.Helper()
	if got := gitOut(t, j.h, j.gateDir, "rev-parse", "refs/heads/"+j.branch); got != j.published {
		t.Errorf("%s: gate lane moved to %s, want %s", label, got, j.published)
	}
	if got := gitOut(t, j.h, j.operator, "rev-parse", "HEAD"); got != j.rebased {
		t.Errorf("%s: local HEAD moved to %s, want %s", label, got, j.rebased)
	}
}

func (j rewrittenJourney) anchor(t *testing.T) (string, bool) {
	t.Helper()
	out, err := j.h.runGit(context.Background(), j.gateDir, "rev-parse", "--verify", "--quiet", custody.RecoveryRef(j.runID))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// TestPrepareRewrittenJourney drives the happy path with the real binary,
// daemon, and local bare push target: status offers the declaration, the
// declaration anchors exactly the published head and moves nothing else, a
// repeat is an idempotent no-op, and the following fresh run archives the old
// lane and fully validates and publishes the rebased head.
func TestPrepareRewrittenJourney(t *testing.T) {
	j := newRewrittenJourney(t, "feature/rewrite")
	h := j.h

	status, err := h.RunInDir(j.operator, "axi", "status")
	saveRewriteEvidence(t, "01-status-before.toon", status)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	for _, want := range []string{"safety: rewrite_preparation_required", "code: prepare_rewritten_submission", "command: no-mistakes axi sync --prepare-rewritten"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}

	out, err := h.RunInDir(j.operator, "axi", "sync", "--prepare-rewritten")
	saveRewriteEvidence(t, "02-prepare-rewritten.toon", out)
	if err != nil {
		t.Fatalf("prepare: %v\n%s", err, out)
	}
	for _, want := range []string{"safety: rewrite_prepared", "changed: true", "code: run_pipeline"} {
		if !strings.Contains(out, want) {
			t.Errorf("prepare output missing %q:\n%s", want, out)
		}
	}
	if got, ok := j.anchor(t); !ok || got != j.published {
		t.Fatalf("anchor = %q (exists %v), want %s", got, ok, j.published)
	}
	j.assertNothingMoved(t, "after prepare")
	if got := h.UpstreamBranchSHA(j.branch); got != j.published {
		t.Fatalf("prepare moved configured target to %s", got)
	}

	again, err := h.RunInDir(j.operator, "axi", "sync", "--prepare-rewritten")
	saveRewriteEvidence(t, "03-prepare-rewritten-repeat.toon", again)
	if err != nil || !strings.Contains(again, "safety: rewrite_prepared") || !strings.Contains(again, "changed: false") {
		t.Fatalf("repeat prepare should be an idempotent no-op: %v\n%s", err, again)
	}

	fresh, err := h.RunInDir(j.operator, "axi", "run", "--yes", "--intent", "validate the deliberately rebased feature")
	saveRewriteEvidence(t, "04-fresh-run.toon", fresh)
	if err != nil || !strings.Contains(fresh, "outcome: passed") {
		t.Fatalf("fresh run after preparation: %v\n%s", err, fresh)
	}
	runs := h.Runs()
	if len(runs) < 2 {
		t.Fatalf("fresh run did not create a new run: %d runs", len(runs))
	}
	if got := h.UpstreamBranchSHA(j.branch); got != j.rebased {
		if _, ancErr := h.runGit(context.Background(), h.UpstreamDir, "merge-base", "--is-ancestor", j.rebased, got); ancErr != nil {
			t.Fatalf("fresh run published %s, which does not carry rebased head %s", got, j.rebased)
		}
	}
	archives := gitOut(t, h, j.gateDir, "for-each-ref", "--points-at", j.published, "--format=%(refname)")
	saveRewriteEvidence(t, "05-gate-refs-pointing-at-old-head.txt", archives+"\n")
	archived := false
	for _, ref := range strings.Split(archives, "\n") {
		if strings.Contains(ref, "archive") || strings.Contains(ref, "abandon") {
			archived = true
		}
	}
	if !archived {
		t.Fatalf("fresh run did not archive the old lane %s; refs at it:\n%s", j.published, archives)
	}
}

// TestPrepareRewrittenRefusesWithoutMutation tries to break the guards: a
// dirty checkout, a configured target that moved, and a rewrite that is
// already published must each refuse without creating the anchor.
func TestPrepareRewrittenRefusesWithoutMutation(t *testing.T) {
	j := newRewrittenJourney(t, "feature/guards")
	h := j.h

	// Dirty checkout.
	if err := os.WriteFile(filepath.Join(j.operator, "feature.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := h.RunInDir(j.operator, "axi", "sync", "--prepare-rewritten")
	saveRewriteEvidence(t, "10-refuse-dirty.toon", dirty)
	if err == nil || !strings.Contains(dirty, "blocked_prepare_rewritten_not_applicable") {
		t.Fatalf("dirty checkout should refuse: %v\n%s", err, dirty)
	}
	if _, ok := j.anchor(t); ok {
		t.Fatal("dirty refusal created the anchor")
	}
	gitOut(t, h, j.operator, "checkout", "--", "feature.txt")
	j.assertNothingMoved(t, "after dirty refusal")

	// Human CLI without a TTY never applies the declaration.
	human, err := h.RunInDir(j.operator, "sync", "--prepare-rewritten")
	saveRewriteEvidence(t, "11-human-non-tty-refuse.txt", human)
	if err == nil || !strings.Contains(human, "--prepare-rewritten --yes") {
		t.Fatalf("non-TTY human prepare should refuse with a --yes hint: %v\n%s", err, human)
	}
	if _, ok := j.anchor(t); ok {
		t.Fatal("non-TTY human refusal created the anchor")
	}

	// Configured target moved by someone else.
	writer := filepath.Join(t.TempDir(), "writer")
	gitOut(t, h, filepath.Dir(writer), "clone", h.UpstreamDir, writer)
	gitOut(t, h, writer, "checkout", j.branch)
	gitOut(t, h, writer, "-c", "user.name=Writer", "-c", "user.email=w@example.com", "commit", "--allow-empty", "-m", "foreign publish")
	gitOut(t, h, writer, "push", "origin", "HEAD:refs/heads/"+j.branch)
	moved, err := h.RunInDir(j.operator, "axi", "sync", "--prepare-rewritten")
	saveRewriteEvidence(t, "12-refuse-target-moved.toon", moved)
	if err == nil || !strings.Contains(moved, "blocked_prepare_rewritten_published_head_mismatch") {
		t.Fatalf("moved target should refuse: %v\n%s", err, moved)
	}
	if _, ok := j.anchor(t); ok {
		t.Fatal("moved-target refusal created the anchor")
	}
	j.assertNothingMoved(t, "after moved-target refusal")

	// The rewrite itself is already published: this declaration is not the
	// right tool and must refuse.
	gitOut(t, h, j.operator, "push", "--force", h.UpstreamDir, "HEAD:refs/heads/"+j.branch)
	already, err := h.RunInDir(j.operator, "axi", "sync", "--prepare-rewritten")
	saveRewriteEvidence(t, "13-refuse-already-published.toon", already)
	if err == nil || !strings.Contains(already, "blocked_prepare_rewritten_published_head_mismatch") {
		t.Fatalf("already-published rewrite should refuse: %v\n%s", err, already)
	}
	if _, ok := j.anchor(t); ok {
		t.Fatal("already-published refusal created the anchor")
	}
	j.assertNothingMoved(t, "after already-published refusal")
}

// TestPrepareRewrittenNotPreparedStillRefusesFreshRun proves archive-before-
// delete reconciliation is unchanged: without the declaration a fresh run of
// the rebased head is still refused and the old lane is left in place.
func TestPrepareRewrittenNotPreparedStillRefusesFreshRun(t *testing.T) {
	j := newRewrittenJourney(t, "feature/unprepared")
	h := j.h
	out, err := h.RunInDir(j.operator, "axi", "run", "--intent", "submit without preparing")
	saveRewriteEvidence(t, "20-unprepared-fresh-run.toon", out)
	if err == nil && strings.Contains(out, "outcome: passed") {
		t.Fatalf("unprepared rewritten submission passed:\n%s", out)
	}
	if got := gitOut(t, h, j.gateDir, "rev-parse", "refs/heads/"+j.branch); got != j.published {
		t.Fatalf("unprepared submission replaced the old gate lane with %s", got)
	}
	if got := h.UpstreamBranchSHA(j.branch); got != j.published {
		t.Fatalf("unprepared submission moved configured target to %s", got)
	}
}
