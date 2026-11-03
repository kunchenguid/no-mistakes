package steps

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCIStep_VerifyApprovalOverride pins CIStep's implementation of
// pipeline.ApprovalOverrideVerifier against the real scm.Host/gh plumbing
// (via the fakecli gh double every other CI step test uses), not just the
// executor-level fake used in internal/pipeline's regression tests. See
// pipeline.ApprovalOverrideVerifier's doc for the incident this exists for:
// a human approving a CI gate must never let a still-failing live check read
// as a clean pass.
func TestCIStep_VerifyApprovalOverride(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		checksJSON     string
		wantUnresolved bool
		wantContains   string
	}{
		{
			name:           "still failing",
			checksJSON:     `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"PR must be raised via no-mistakes","state":"FAILURE","bucket":"fail"}]`,
			wantUnresolved: true,
			wantContains:   "PR must be raised via no-mistakes",
		},
		{
			name:           "became green",
			checksJSON:     `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"PR must be raised via no-mistakes","state":"SUCCESS","bucket":"pass"}]`,
			wantUnresolved: false,
		},
		// Regression for the upstream review P1 "unresolved checks become
		// clean passes": before this fix, VerifyApprovalOverride used
		// !hasFailingChecks, which reads pending/cancelled/unknown-bucket
		// checks (none of them Failing()) as a clean pass. It must instead
		// use allChecksPassed, the same trusted all-green semantics the CI
		// step's own polling loop uses, so anything short of every check
		// being pass/skip is reported as unresolved.
		{
			name:           "still pending",
			checksJSON:     `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"deploy","state":"IN_PROGRESS","bucket":"pending"}]`,
			wantUnresolved: true,
			wantContains:   "deploy",
		},
		{
			name:           "cancelled",
			checksJSON:     `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"deploy","state":"CANCELLED","bucket":"cancel"}]`,
			wantUnresolved: true,
			wantContains:   "deploy",
		},
		{
			// The verifier now names the delivered commit, so this reads the
			// commit's check rollup, and that read refuses a context whose state
			// it cannot bucket rather than carrying it forward: still unresolved,
			// never a clean pass. The bucket mapping itself stays covered below,
			// where the GitLab reader keeps an unrecognized status visible.
			name:           "unknown bucket refuses the commit read",
			checksJSON:     `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"legacy","state":"SOMETHING_NEW","bucket":"weird"}]`,
			wantUnresolved: true,
			wantContains:   "incomplete context",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The verifier names the delivered commit on every read now, so the
			// fixture is a run worktree whose HEAD is that commit, the same setup
			// the CI monitor tests use.
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			env := fakeCIGH(t, "OPEN", tc.checksJSON)

			prURL := "https://github.com/test/repo/pull/42"
			sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = env
			sctx.Run.PRURL = &prURL

			step := &CIStep{}
			unresolved, err := step.VerifyApprovalOverride(sctx)
			if err != nil {
				t.Fatalf("VerifyApprovalOverride() error = %v", err)
			}
			if tc.wantUnresolved && unresolved == "" {
				t.Fatal("unresolved = \"\", want a reason naming the still-failing check")
			}
			if !tc.wantUnresolved && unresolved != "" {
				t.Fatalf("unresolved = %q, want \"\" once every check passed", unresolved)
			}
			if tc.wantContains != "" && !strings.Contains(unresolved, tc.wantContains) {
				t.Errorf("unresolved = %q, want it to name %q", unresolved, tc.wantContains)
			}
		})
	}
}

// TestCIStep_VerifyApprovalOverride_NoPRURL covers the "cannot verify" fail-
// closed path: a run with no PR URL yet cannot have a live state to check
// against, so this must report an unresolved reason (never silently clear),
// matching ApprovalOverrideVerifier's documented fail-closed contract.
func TestCIStep_VerifyApprovalOverride_NoPRURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sctx := newTestContext(t, nil, dir, "base", "deadbeef", config.Commands{})

	step := &CIStep{}
	unresolved, err := step.VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatalf("VerifyApprovalOverride() error = %v", err)
	}
	if unresolved == "" {
		t.Fatal("unresolved = \"\", want a fail-closed reason when there is no PR URL to verify")
	}
}

// TestCIStep_VerifyApprovalOverride_EmptyChecks is the other half of the
// upstream review P1 "unresolved checks become clean passes": before this
// fix, !hasFailingChecks(nil) is true (an empty slice contains no failing
// check), so a PR reporting zero live checks at all read as a clean pass.
// allChecksPassed correctly treats an empty check list as NOT passed, and
// VerifyApprovalOverride must report that as unresolved rather than clear.
func TestCIStep_VerifyApprovalOverride_EmptyChecks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	env := fakeCIGHNoChecks(t)

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL

	step := &CIStep{}
	unresolved, err := step.VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatalf("VerifyApprovalOverride() error = %v", err)
	}
	if unresolved == "" {
		t.Fatal("unresolved = \"\", want a fail-closed reason when the PR reports no checks at all")
	}
}

// fakeCIGlabOverride serves the GitLab CI endpoints for the override verifier:
// the merge request reports headSHA as its source revision and ran its head
// pipeline at pipelineSHA. A pipelineSHA different from headSHA is the stale or
// stranded pipeline the verifier must refuse.
func fakeCIGlabOverride(t *testing.T, headSHA, pipelineSHA, checksJSON string) []string {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "glab")
	return fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":         "ci-glab",
		"FAKE_CLI_STATE":        "opened",
		"FAKE_CLI_CHECKS":       checksJSON,
		"FAKE_CLI_MR_HEAD_SHA":  headSHA,
		"FAKE_CLI_PIPELINE_SHA": pipelineSHA,
	})
}

// TestCIStep_VerifyApprovalOverride_NamesTheDeliveredCommit covers the
// verifier's own read on a host that binds checks to a named commit. The
// delivered commit must be named there too: a green pipeline that ran at
// another commit is another commit's checks, so it must refuse (recording an
// override) rather than read as a clean pass, and the refusal must not be
// reported as a provider or CLI failure.
func TestCIStep_VerifyApprovalOverride_NamesTheDeliveredCommit(t *testing.T) {
	t.Parallel()

	t.Run("stranded pipeline refuses", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		env := fakeCIGlabOverride(t, "deadbeef", strings.Repeat("b", 40), `[{"id":1,"name":"build","status":"success"}]`)

		prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
		sctx := newTestContext(t, nil, dir, "base", "deadbeef", config.Commands{})
		sctx.Env = env
		sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
		sctx.Run.PRURL = &prURL

		unresolved, err := (&CIStep{}).VerifyApprovalOverride(sctx)
		if err != nil {
			t.Fatalf("VerifyApprovalOverride() error = %v", err)
		}
		if unresolved == "" {
			t.Fatal("unresolved = \"\", want an override reason: a pipeline that ran at another commit is not this commit's green")
		}
		if strings.Contains(unresolved, "could not verify live CI state") {
			t.Fatalf("unresolved = %q, want the head-binding refusal, not a broken-tool message", unresolved)
		}
		if !strings.Contains(unresolved, "not for the commit being delivered") {
			t.Fatalf("unresolved = %q, want it to name the head-binding refusal", unresolved)
		}
	})

	t.Run("unrecognized job status is unresolved and named", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		env := fakeCIGlabOverride(t, "deadbeef", "deadbeef",
			`[{"id":1,"name":"build","status":"success"},{"id":2,"name":"legacy","status":"something_new"}]`)

		prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
		sctx := newTestContext(t, nil, dir, "base", "deadbeef", config.Commands{})
		sctx.Env = env
		sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
		sctx.Run.PRURL = &prURL

		unresolved, err := (&CIStep{}).VerifyApprovalOverride(sctx)
		if err != nil {
			t.Fatalf("VerifyApprovalOverride() error = %v", err)
		}
		if !strings.Contains(unresolved, "legacy") {
			t.Fatalf("unresolved = %q, want it to name the check whose status could not be bucketed", unresolved)
		}
	})

	t.Run("pipeline at the delivered commit is a clean pass", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		env := fakeCIGlabOverride(t, "deadbeef", "deadbeef", `[{"id":1,"name":"build","status":"success"}]`)

		prURL := "https://gitlab.com/test/repo/-/merge_requests/42"
		sctx := newTestContext(t, nil, dir, "base", "deadbeef", config.Commands{})
		sctx.Env = env
		sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
		sctx.Run.PRURL = &prURL

		unresolved, err := (&CIStep{}).VerifyApprovalOverride(sctx)
		if err != nil {
			t.Fatalf("VerifyApprovalOverride() error = %v", err)
		}
		if unresolved != "" {
			t.Fatalf("unresolved = %q, want \"\" when the delivered commit's own pipeline is green", unresolved)
		}
	})
}

// headRewriteParkFixture is a run parked on a published head rewrite: the push
// target serves a commit the run never validated, which is what the override
// reason has to name. The remote is the source of that head for every
// provider, so this fixture is provider-independent the way the park itself is.
func headRewriteParkFixture(t *testing.T, prState string) (*pipeline.StepContext, string, string) {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "validated")
	validated := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "feature")

	// The branch is rewritten outside the run, exactly as a native stack rebase
	// does it: the push target now serves a commit the run never validated.
	tree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
	foreign := gitCmd(t, dir, "commit-tree", tree, "-p", validated, "-m", "rewritten outside the run")
	gitCmd(t, dir, "push", "--force", "origin", foreign+":refs/heads/feature")

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, nil, dir, validated, validated, config.Commands{})
	sctx.Env = append(fakeCIGH(t, prState, `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`), "FAKE_CLI_PR_HEAD_SHA="+validated)
	sctx.Run.PRURL = &prURL
	sctx.Run.Branch = "refs/heads/feature"

	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, validated); err != nil {
		t.Fatal(err)
	}

	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	park := parkPublishedHeadRewrite(validated, validated, foreign, "the run worktree holds commits that are neither on the live head nor recorded as published")
	if err := sctx.DB.SetStepFindings(stepResult.ID, park.Findings); err != nil {
		t.Fatal(err)
	}
	return sctx, validated, foreign
}

// A human may approve the published-head-rewrite park - that is the verdict the
// gate offers - but the checks being approved are the forge's for a head this
// run validated nowhere, so the completion must be recorded as
// passed-with-override however green they are. Before this, green checks for the
// foreign head returned "" and the run completed reporting CI passed for a
// commit Review and Test never saw. The reason names the head the PUSH TARGET
// serves, which every provider reports the same way: reading it back out of the
// PR struct the caller had just filled with the run's own head named that head
// twice on every provider but GitHub.
func TestCIStep_VerifyApprovalOverride_HeadRewriteParkIsNeverACleanPass(t *testing.T) {
	t.Parallel()

	sctx, validated, foreign := headRewriteParkFixture(t, "OPEN")

	unresolved, err := (&CIStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatalf("VerifyApprovalOverride() error = %v", err)
	}
	if unresolved == "" {
		t.Fatal("unresolved = \"\", want the head rewrite recorded as an unresolved condition despite the green checks")
	}
	if !strings.Contains(unresolved, shortSHA(validated)) || !strings.Contains(unresolved, shortSHA(foreign)) {
		t.Fatalf("unresolved = %q, want it to name the validated head %s and the published head %s", unresolved, shortSHA(validated), shortSHA(foreign))
	}
	if strings.Count(unresolved, shortSHA(validated)) != 1 {
		t.Fatalf("unresolved = %q, names the run's own head where the published head belongs", unresolved)
	}
}

// A pull request merged at a head the run OWNS resolves the park and is an
// ordinary clean pass: the merge proof has just shown the rewrite resolved, so
// stamping passed-with-override would record "this run owns nowhere" about a
// head it had been proved to own - naming the same SHA twice when the merged
// head is the validated one.
func TestCIStep_ReconcileApprovalGate_MergedAtAnOwnedHeadRecordsNoOverride(t *testing.T) {
	t.Parallel()

	sctx, validated, _ := headRewriteParkFixture(t, "MERGED")
	// The run also published a later head, and that is the one the pull
	// request was merged at: an own head, so the merge proof accepts it.
	mergedHead := gitCmd(t, sctx.WorkDir, "commit-tree", gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD^{tree}"), "-p", validated, "-m", "published later")
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{
		HeadSHA: mergedHead, TargetKind: "upstream",
		TargetFingerprint: branchsync.TargetFingerprint(sctx.Repo.UpstreamURL), Ref: "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}
	sctx.Env = append(sctx.Env, "FAKE_CLI_PR_HEAD_SHA="+mergedHead)

	resolved, err := (&CIStep{}).ReconcileApprovalGate(sctx)
	if err != nil || !resolved {
		t.Fatalf("ReconcileApprovalGate() = (%v, %v), want the merged PR to resolve the gate", resolved, err)
	}
	after, err := sctx.DB.GetStepResult(sctx.StepResultID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OverrideReason != nil {
		t.Fatalf("override reason = %q, want none: the PR merged at a head this run owns", *after.OverrideReason)
	}
}

// The same rule on the approval path: an operator who restores the branch to
// the run's own validated head before approving gets the ordinary live-check
// verification, not a head-rewrite override naming that head as foreign.
func TestCIStep_VerifyApprovalOverride_HeadRewriteResolvedOntoAnOwnedHead(t *testing.T) {
	t.Parallel()

	sctx, validated, _ := headRewriteParkFixture(t, "OPEN")
	// The branch is force-pushed back to the head the run validated.
	gitCmd(t, sctx.WorkDir, "push", "--force", "origin", validated+":refs/heads/feature")

	unresolved, err := (&CIStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatalf("VerifyApprovalOverride() error = %v", err)
	}
	if unresolved != "" {
		t.Fatalf("unresolved = %q, want the green live checks to pass cleanly once the branch is back on the validated head", unresolved)
	}
}

// The same invariant on the reconciliation path: a PR closed while the gate sat
// parked on a head rewrite resolves the gate, but the completion carries the
// override record rather than reading as an ordinary pass.
func TestCIStep_ReconcileApprovalGate_ClosedPRRecordsTheHeadRewriteOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	prURL := "https://github.com/test/repo/pull/42"
	const foreign = "1111111111111111111111111111111111111111"
	sctx := newTestContextWithDBRecords(t, nil, dir, headSHA, headSHA, config.Commands{})
	sctx.Env = fakeCIGH(t, "CLOSED", `[]`)
	sctx.Run.PRURL = &prURL

	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	park := parkPublishedHeadRewrite(headSHA, headSHA, foreign, "the run worktree holds commits that are neither on the live head nor recorded as published")
	if err := sctx.DB.SetStepFindings(stepResult.ID, park.Findings); err != nil {
		t.Fatal(err)
	}

	resolved, err := (&CIStep{}).ReconcileApprovalGate(sctx)
	if err != nil || !resolved {
		t.Fatalf("ReconcileApprovalGate() = (%v, %v), want the closed PR to resolve the gate", resolved, err)
	}
	after, err := sctx.DB.GetStepResult(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OverrideReason == nil || !strings.Contains(*after.OverrideReason, "owns nowhere") {
		t.Fatalf("override reason = %v, want the head rewrite recorded as unresolved", after.OverrideReason)
	}
}

// A pull request merged at a head the run owns nowhere is the same refusal the
// poll loop produces, and it is deterministic: the proof reads the same on every
// tick. Returned as a plain error it preserved the gate and retried forever, so
// the run never reached a terminal outcome; it must fail the run fatally.
func TestCIStep_ReconcileApprovalGate_MergedAtAForeignHeadFailsFatally(t *testing.T) {
	t.Parallel()

	sctx, _, foreign := headRewriteParkFixture(t, "MERGED")
	// The forge reports the merge at the foreign head, which is in no owned set.
	sctx.Env = append(sctx.Env, "FAKE_CLI_PR_HEAD_SHA="+foreign)

	resolved, err := (&CIStep{}).ReconcileApprovalGate(sctx)
	if resolved {
		t.Fatalf("ReconcileApprovalGate() resolved = true, want the merge refused")
	}
	if !errors.Is(err, pipeline.ErrFatalGateReconciliation) {
		t.Fatalf("error = %v, want a fatal reconciliation so the run ends rather than retrying forever", err)
	}
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("error = %v, want the pull-request-head-changed refusal preserved", err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRState != nil && *run.PRState == "merged" {
		t.Fatal("PR state recorded as merged for a head the run owns nowhere")
	}
}

// A human may approve the unowned-head stall park, and that approval must carry
// the same unresolved-condition record the rewrite park's does: the checks it
// would otherwise pass are the forge's for a head this run validated nowhere.
func TestCIStep_VerifyApprovalOverride_UnownedHeadStallIsNeverACleanPass(t *testing.T) {
	t.Parallel()

	sctx, validated, foreign := headRewriteParkFixture(t, "OPEN")
	stall := ciUnownedHeadStallOutcome()
	if err := sctx.DB.SetStepFindings(sctx.StepResultID, stall.Findings); err != nil {
		t.Fatal(err)
	}

	unresolved, err := (&CIStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatalf("VerifyApprovalOverride() error = %v", err)
	}
	if unresolved == "" {
		t.Fatal("unresolved = \"\", want the stall recorded as an unresolved condition despite the green checks")
	}
	if !strings.Contains(unresolved, shortSHA(validated)) || !strings.Contains(unresolved, shortSHA(foreign)) {
		t.Fatalf("unresolved = %q, want it to name the validated head %s and the published head %s", unresolved, shortSHA(validated), shortSHA(foreign))
	}
}

func TestCIStep_ReconcileApprovalGate_TransientMergedProofKeepsTheGate(t *testing.T) {
	t.Parallel()
	sctx, _, _ := headRewriteParkFixture(t, "MERGED")
	sctx.Env = append(sctx.Env, "FAKE_CLI_PR_MERGED_PROOF_ERR=rate limit")
	resolved, err := (&CIStep{}).ReconcileApprovalGate(sctx)
	if resolved || err == nil || errors.Is(err, pipeline.ErrFatalGateReconciliation) {
		t.Fatalf("reconcile = %v, %v; want a retryable read failure", resolved, err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRState != nil && *run.PRState == "merged" {
		t.Fatal("a failed proof recorded the PR as merged")
	}
}
