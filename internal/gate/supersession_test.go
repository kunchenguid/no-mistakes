package gate

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// supersededMirrorChain reproduces the recorded stranded-mirror chain: a private
// mirror left on a superseded lineage whose content the live head cannot prove
// preserved, with exactly one blob genuinely absent tree-wide.
//
// Shape recorded on run 01M2P0H3ADCA1T65W0GJTASRNR (branch
// fm/controller-daily-repair-session-q1): mirror 747927b002, terminal head
// fbdb8d144, remote 30c52267; the mirror head is an ancestor of neither, and
// `git merge-tree --write-tree` conflicts in exactly the file the run's own
// accepted fix superseded. The fixture keeps that structure - a divergent private
// lineage, a patch-equivalent pair, and one intermediate blob the accepted result
// replaced - at a size a test can build.
//
// The working directory is a worktree of the gate, which is how the pipeline
// really runs: run worktrees are carved from the gate, so every head a run
// produced is present in the gate's object store even when no ref names it. That
// is why the terminal accepted head is readable in the gate below.
type supersededMirrorChain struct {
	work     string // clean local head, descending from the accepted head
	gateDir  string
	branch   string
	base     string
	private  string // the mirror head: the head this run submitted
	accepted string // the run's own accepted, reviewed result
	// continued is a post-review continuation of accepted: the terminal shape
	// whose verified final head carries commits made after the reviewed head.
	continued string
	live      string // the current clean local head
}

func newSupersededMirrorChain(t *testing.T) *supersededMirrorChain {
	t.Helper()
	root := t.TempDir()
	gateDir := filepath.Join(root, "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)

	work := filepath.Join(root, "runwork")
	reconcileGit(t, gateDir, "worktree", "add", "-b", "tmp-run", work)
	reconcileGit(t, work, "config", "user.name", "test")
	reconcileGit(t, work, "config", "user.email", "test@example.com")

	writeReconcileFile(t, work, "base.txt", "base\n")
	reconcileGit(t, work, "add", "base.txt")
	reconcileGit(t, work, "commit", "-m", "base")
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	// The private lineage: an intermediate version of a test file, then the
	// substantive change, mirroring the recorded private-only commits.
	writeReconcileFile(t, work, "DailyRepairRepository.test.ts", "intermediate assertion\n")
	reconcileGit(t, work, "add", "DailyRepairRepository.test.ts")
	reconcileGit(t, work, "commit", "-m", "test(feeds): probe SQL text")
	writeReconcileFile(t, work, "repair.go", "package repair\n\nfunc Run() error { return nil }\n")
	reconcileGit(t, work, "add", "repair.go")
	reconcileGit(t, work, "commit", "-m", "feat(feeds): stage reconciliation")
	private := reconcileGit(t, work, "rev-parse", "HEAD")

	// The accepted result: the run's own gate fix supersedes the intermediate
	// assertion, so exactly that blob is absent from the live lineage.
	reconcileGit(t, work, "reset", "--hard", base)
	writeReconcileFile(t, work, "DailyRepairRepository.test.ts", "final assertion\n")
	reconcileGit(t, work, "add", "DailyRepairRepository.test.ts")
	reconcileGit(t, work, "commit", "-m", "test(feeds): assert staged-reconciliation behavior")
	accepted := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "repair.go", "package repair\n\nfunc Run() error { return nil }\n")
	reconcileGit(t, work, "add", "repair.go")
	reconcileGit(t, work, "commit", "-m", "feat(feeds): stage reconciliation")
	live := reconcileGit(t, work, "rev-parse", "HEAD")

	// The post-review continuation shape: a document/lint or CI-repair round
	// commits AFTER review, so the run's verified final head differs from the
	// head review approved.
	reconcileGit(t, work, "reset", "--hard", accepted)
	writeReconcileFile(t, work, "notes.md", "post-review housekeeping\n")
	reconcileGit(t, work, "add", "notes.md")
	reconcileGit(t, work, "commit", "-m", "docs: post-review housekeeping")
	continued := reconcileGit(t, work, "rev-parse", "HEAD")

	// The mirror holds the superseded submitted head.
	reconcileGit(t, gateDir, "update-ref", "refs/heads/feature/stranded", private)

	chain := &supersededMirrorChain{
		work: work, gateDir: gateDir, branch: "feature/stranded",
		base: base, private: private, accepted: accepted, continued: continued, live: live,
	}
	for _, head := range []string{private, live} {
		if _, err := reconcileGitErr(chain.gateDir, "merge-base", "--is-ancestor", head, live); err == nil && head == private {
			t.Fatal("fixture must reproduce a private head that is not an ancestor of the live head")
		}
	}
	if _, err := reconcileGitErr(chain.gateDir, "merge-base", "--is-ancestor", private, live); err == nil {
		t.Fatal("fixture must reproduce a private head that is not an ancestor of the live head")
	}
	if _, err := reconcileGitErr(chain.gateDir, "merge-base", "--is-ancestor", live, private); err == nil {
		t.Fatal("fixture must reproduce a live head that is not an ancestor of the private head")
	}
	return chain
}

// reconcileGitErr runs git and returns its combined output with the error, for
// fixtures and tests that need a command which may fail.
func reconcileGitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// terminalRunOptions selects which recorded evidence exists, so a test can prove
// that removing any single field restores the refusal.
type terminalRunOptions struct {
	// custodyReturned false leaves the run still owning the branch.
	custodyReturned bool
	// submittedHead overrides the head the run records as submitted.
	submittedHead string
	// reviewApprovedHead overrides the run's review authority; empty omits it.
	reviewApprovedHead string
	// verifiedHead overrides the verified final head the terminal status records,
	// which is how the post-review-continuation shape is recorded.
	verifiedHead string
}

// terminalRun records the terminal run that submitted the mirror head and
// accepted a reviewed result, honoring the requested evidence shape.
func (c *supersededMirrorChain) terminalRun(t *testing.T, d *db.DB, repoID string, opts terminalRunOptions) *db.Run {
	t.Helper()
	submitted := opts.submittedHead
	if submitted == "" {
		submitted = c.private
	}
	verified := opts.verifiedHead
	if verified == "" {
		verified = c.accepted
	}
	run, err := d.InsertRun(repoID, c.branch, submitted, c.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, verified); err != nil {
		t.Fatal(err)
	}
	if approved := opts.reviewApprovedHead; approved != "" {
		if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, approved); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, verified); err != nil {
		t.Fatal(err)
	}
	if opts.custodyReturned {
		if err := d.SetRunCustodyReturned(run.ID); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored == nil {
		t.Fatalf("read back run: %v", err)
	}
	return stored
}

// acceptedRun records the ordinary, fully-proven terminal run.
func (c *supersededMirrorChain) acceptedRun(t *testing.T, d *db.DB, repoID string) *db.Run {
	t.Helper()
	return c.terminalRun(t, d, repoID, terminalRunOptions{
		custodyReturned:    true,
		reviewApprovedHead: c.accepted,
	})
}

// sourceFor builds the evidence source the submission paths use.
func (c *supersededMirrorChain) sourceFor(repoID string, d *db.DB) SupersessionSource {
	return SupersessionSource{DB: d, RepoID: repoID, Branch: c.branch}
}

// mirrorHead reads the mirror ref, so every assertion can prove the refusal left
// it exactly where it was.
func (c *supersededMirrorChain) mirrorHead(t *testing.T) string {
	t.Helper()
	return reconcileGit(t, c.gateDir, "rev-parse", "refs/heads/"+c.branch)
}

func (c *supersededMirrorChain) archiveTags(t *testing.T) string {
	t.Helper()
	return reconcileGit(t, c.gateDir, "tag", "--list", "no-mistakes-abandoned/*")
}

// TestStaleDivergentMirrorDeadlockIsPinnedAndResolvedByRecordedEvidence is the
// defect's regression: on the recorded chain a fresh submission is refused by the
// containment guard alone, and recorded run evidence resolves that exact state
// without weakening the refusal.
func TestStaleDivergentMirrorDeadlockIsPinnedAndResolvedByRecordedEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	chain := newSupersededMirrorChain(t)

	// Before: the exact call `axi run` makes today (no run-owned head, no
	// evidence) refuses. This is the deadlock - the local head cannot be
	// submitted and no command settles the mirror.
	if _, err := ReconcileStaleBranch(ctx, chain.gateDir, chain.work, chain.branch, chain.live, ""); err == nil {
		t.Fatal("a fresh submission reconciled the divergent mirror; the preservation guarantee is not being exercised")
	}
	plan, err := PlanSupersededSubmissionReconciliation(ctx, chain.gateDir, chain.work, chain.branch, chain.live, SupersessionSource{})
	var refusal *StaleBranchRefusal
	if err == nil || !errors.As(err, &refusal) {
		t.Fatalf("guard did not refuse the divergent mirror: plan=%+v err=%v", plan, err)
	}
	if len(refusal.AtRisk) == 0 || !strings.Contains(err.Error(), chain.private) {
		t.Fatalf("refusal did not name the at-risk private commit: %v", err)
	}
	if plan.Reconcile {
		t.Fatalf("refusal reported reconciliation: %+v", plan)
	}
	if got := chain.mirrorHead(t); got != chain.private {
		t.Fatalf("refusal moved the mirror to %s, want %s untouched", got, chain.private)
	}
	if tags := chain.archiveTags(t); tags != "" {
		t.Fatalf("refusal archived a mirror it did not prove: %q", tags)
	}
	if action := refusal.Action(); action == "" {
		t.Fatal("refusal carried no operator action, leaving the branch stranded")
	}
	t.Logf("before: refused, mirror untouched at %s, %d at-risk commit(s), action=%q", chain.private, len(refusal.AtRisk), refusal.Action())

	// After: the same state, proven from this repository's own records.
	d := openTestDB(t, testPaths(t))
	repo, err := d.InsertRepo(chain.work, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run := chain.acceptedRun(t, d, repo.ID)

	result, err := ReconcileSupersededSubmission(ctx, chain.gateDir, chain.work, chain.branch, chain.live, chain.sourceFor(repo.ID, d))
	if err != nil {
		t.Fatalf("recorded evidence did not resolve the stranded mirror: %v", err)
	}
	if !result.Reconciled || result.PreviousHead != chain.private {
		t.Fatalf("reconciliation = %+v, want the private head %s archived", result, chain.private)
	}
	// The superseded private head survives in the archive, so the work it held is
	// recorded rather than discarded, and the live head can then enter normally.
	if got := reconcileGit(t, chain.gateDir, "rev-parse", result.ArchivedTag+"^{commit}"); got != chain.private {
		t.Fatalf("archive points at %s, want %s", got, chain.private)
	}
	if _, err := reconcileGitErr(chain.gateDir, "rev-parse", "--verify", "refs/heads/"+chain.branch); err == nil {
		t.Fatal("reconciled branch ref still exists; the live head cannot enter until it does")
	}
	// The live head now enters through an ordinary new-branch push, which is the
	// whole point of settling the mirror: no force is needed.
	reconcileGit(t, chain.work, "push", chain.gateDir, chain.live+":refs/heads/"+chain.branch)
	if got := chain.mirrorHead(t); got != chain.live {
		t.Fatalf("ordinary push reached %s, want %s", got, chain.live)
	}
	t.Logf("after: run %s submitted %s, accepted %s survived in %s; ordinary push reached %s", run.ID, chain.private, chain.accepted, chain.live, chain.live)
}

// TestStaleDivergentMirrorNamesPostReviewContinuationRefusal is the second
// recorded shape's regression: a terminal run whose verified final head carries
// commits made after review cannot use recorded supersession (that exception
// requires the verified head to equal the reviewed head), and the run already
// returned custody, so neither `axi sync --recover` nor the `axi run` this state
// reports can settle the mirror. The refusal must therefore name that exact
// condition and the step that actually resolves it - bringing the mirror's own
// commits into the submitted head - while still moving no ref and still refusing.
func TestStaleDivergentMirrorNamesPostReviewContinuationRefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	chain := newSupersededMirrorChain(t)
	d := openTestDB(t, testPaths(t))
	repo, err := d.InsertRepo(chain.work, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	// The recorded continuation: submitted exactly the mirror head, reviewed the
	// accepted head, then committed a post-review round, then returned custody.
	chain.terminalRun(t, d, repo.ID, terminalRunOptions{
		custodyReturned:    true,
		reviewApprovedHead: chain.accepted,
		verifiedHead:       chain.continued,
	})

	plan, err := PlanSupersededSubmissionReconciliation(ctx, chain.gateDir, chain.work, chain.branch, chain.live, chain.sourceFor(repo.ID, d))
	var refusal *StaleBranchRefusal
	if err == nil || !errors.As(err, &refusal) {
		t.Fatalf("guard did not refuse the post-review continuation: plan=%+v err=%v", plan, err)
	}
	if plan.Reconcile {
		t.Fatalf("post-review continuation reported as reconcilable: %+v", plan)
	}
	// The replacement is refused, so the diagnosis must never read as authority.
	if refusal.Continuation == nil {
		t.Fatal("refusal did not name the continuation it was in")
	}
	if refusal.Continuation.RunID == "" || refusal.Continuation.VerifiedHead != chain.continued || refusal.Continuation.ReviewedHead != chain.accepted {
		t.Fatalf("continuation = %+v, want run naming verified %s and reviewed %s", refusal.Continuation, chain.continued, chain.accepted)
	}
	for _, want := range []string{chain.continued, chain.accepted} {
		if !strings.Contains(refusal.Condition(), want) {
			t.Errorf("condition %q does not name %s", refusal.Condition(), want)
		}
	}
	if !strings.Contains(refusal.Error(), refusal.Condition()) {
		t.Errorf("Error() = %q does not carry the continuation condition", refusal.Error())
	}
	// The generic dead-end action points at `axi status`, which in this state
	// offers `axi run` - the very entry point that refuses. The continuation
	// refusal must instead name a step that is reachable here.
	action := refusal.Action()
	if strings.Contains(action, "branch_sync.next_action.command") {
		t.Errorf("action %q still offers the branch_sync dead-end for a returned-custody run", action)
	}
	if !strings.Contains(action, RemoteName) || !strings.Contains(action, chain.branch) {
		t.Errorf("action %q does not name the gate remote and branch to fetch the mirror commits from", action)
	}
	// The preservation guarantee is untouched: no ref moved, nothing archived.
	if got := chain.mirrorHead(t); got != chain.private {
		t.Fatalf("refusal moved the mirror to %s, want %s untouched", got, chain.private)
	}
	if tags := chain.archiveTags(t); tags != "" {
		t.Fatalf("refusal archived a mirror it did not prove: %q", tags)
	}

	// The named step really settles it: once the submitted head contains the
	// mirror's commits, the same submission is an ordinary fast-forward. The
	// private lineage conflicts with the accepted result exactly where the
	// superseded intermediate assertion sits, so this is the resolution an
	// operator performs.
	reconcileGit(t, chain.work, "reset", "--hard", chain.live)
	if _, err := reconcileGitErr(chain.work, "merge", "--no-edit", chain.private); err == nil {
		t.Fatal("fixture expected the mirror lineage to conflict with the accepted result")
	}
	writeReconcileFile(t, chain.work, "DailyRepairRepository.test.ts", "final assertion\n")
	reconcileGit(t, chain.work, "add", "DailyRepairRepository.test.ts")
	reconcileGit(t, chain.work, "commit", "--no-edit")
	settled := reconcileGit(t, chain.work, "rev-parse", "HEAD")
	if _, err := reconcileGitErr(chain.gateDir, "merge-base", "--is-ancestor", chain.private, settled); err != nil {
		t.Fatalf("resolved head %s does not contain the mirror head %s", settled, chain.private)
	}
	plan, err = PlanSupersededSubmissionReconciliation(ctx, chain.gateDir, chain.work, chain.branch, settled, chain.sourceFor(repo.ID, d))
	if err != nil {
		t.Fatalf("submitting a head that contains the mirror's commits was still refused: %v", err)
	}
	if plan.Reconcile {
		t.Fatalf("an ancestor mirror reported as needing reconciliation: %+v", plan)
	}
}

// TestStaleDivergentMirrorStillRefusesWithoutRecordedEvidence is the preservation
// guarantee: every way of lacking the recorded proof keeps the refusal, and none
// of them moves a ref.
func TestStaleDivergentMirrorStillRefusesWithoutRecordedEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, variant := range []struct {
		name string
		opts terminalRunOptions
		// absentFromLive rebuilds the local head so it stops containing the
		// accepted result.
		absentFromLive bool
		// noRunRecords inserts no run at all.
		noRunRecords bool
	}{
		{name: "no_run_records", noRunRecords: true},
		{name: "run_still_owns_the_branch", opts: terminalRunOptions{reviewApprovedHead: "accepted"}},
		{name: "submitted_head_differs", opts: terminalRunOptions{custodyReturned: true, reviewApprovedHead: "accepted", submittedHead: "base"}},
		{name: "accepted_head_absent_from_live", opts: terminalRunOptions{custodyReturned: true, reviewApprovedHead: "accepted"}, absentFromLive: true},
		{name: "accepted_head_not_reviewed", opts: terminalRunOptions{custodyReturned: true}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			t.Parallel()
			chain := newSupersededMirrorChain(t)
			d := openTestDB(t, testPaths(t))
			repo, err := d.InsertRepo(chain.work, "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			if !variant.noRunRecords {
				opts := variant.opts
				if opts.submittedHead == "base" {
					opts.submittedHead = chain.base
				}
				if opts.reviewApprovedHead == "accepted" {
					opts.reviewApprovedHead = chain.accepted
				}
				chain.terminalRun(t, d, repo.ID, opts)
			}
			if variant.absentFromLive {
				// Live work unrelated to the accepted result, so that result is
				// not contained in what would be submitted.
				reconcileGit(t, chain.work, "reset", "--hard", chain.base)
				writeReconcileFile(t, chain.work, "unrelated.txt", "unrelated\n")
				reconcileGit(t, chain.work, "add", "unrelated.txt")
				reconcileGit(t, chain.work, "commit", "-m", "unrelated live work")
				chain.live = reconcileGit(t, chain.work, "rev-parse", "HEAD")
			}

			plan, err := PlanSupersededSubmissionReconciliation(ctx, chain.gateDir, chain.work, chain.branch, chain.live, chain.sourceFor(repo.ID, d))
			if err == nil {
				t.Fatalf("reconciled %s without recorded proof: plan=%+v", variant.name, plan)
			}
			if plan.Reconcile {
				t.Fatalf("unproven private head reported as reconcilable: %+v", plan)
			}
			var refusal *StaleBranchRefusal
			if !errors.As(err, &refusal) || len(refusal.AtRisk) == 0 {
				t.Fatalf("refusal did not report at-risk commits: %v", err)
			}
			if got := chain.mirrorHead(t); got != chain.private {
				t.Fatalf("refusal moved the mirror to %s, want %s untouched", got, chain.private)
			}
			if tags := chain.archiveTags(t); tags != "" {
				t.Fatalf("refusal archived a mirror it did not prove: %q", tags)
			}
		})
	}
}

// testPaths returns an isolated paths root for a test database.
func testPaths(t *testing.T) *paths.Paths {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	return p
}
