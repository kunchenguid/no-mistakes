package steps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunPushBranch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  *db.Run
		want string
	}{
		{"nil run", nil, ""},
		{"branch falls through", &db.Run{Branch: "feature"}, "feature"},
		{"refs/heads prefix normalized", &db.Run{Branch: "refs/heads/feature"}, "feature"},
		{"bound push branch wins", &db.Run{Branch: "refs/heads/feature", PushBranch: strptr("stacked-pr")}, "stacked-pr"},
		{"blank push branch falls back", &db.Run{Branch: "feature", PushBranch: strptr("   ")}, "feature"},
		{"push branch trimmed", &db.Run{Branch: "feature", PushBranch: strptr(" stacked-pr ")}, "stacked-pr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runPublishBranch(tc.run); got != tc.want {
				t.Fatalf("runPublishBranch = %q, want %q", got, tc.want)
			}
		})
	}
	if got := runPushBranch(nil); got != "" {
		t.Fatalf("runPushBranch(nil) = %q, want empty", got)
	}
	sctx := &pipeline.StepContext{Run: &db.Run{Branch: "feature", PushBranch: strptr("bound")}}
	if got := runPushBranch(sctx); got != "bound" {
		t.Fatalf("runPushBranch = %q, want bound", got)
	}
}

func TestValidateRunPushBranchName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"   ", "", false},
		{"feature/x", "feature/x", false},
		{" stacked-pr ", "stacked-pr", false},
		{"refs/heads/feature", "", true},
		{"-steal-flags", "", true},
		{"a..b", "", true},
		{"@{upstream}", "", true},
		{"HEAD", "", true},
	}
	for _, tc := range cases {
		got, err := ValidateRunPushBranchName(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ValidateRunPushBranchName(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ValidateRunPushBranchName(%q) error = %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ValidateRunPushBranchName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPushStep_PublishesToBoundPushBranch exercises the --push-branch bind end
// to end: the verified head lands on the bound remote ref, the gate mirror
// keeps tracking the run's own branch identity, and the recorded push binding
// names the bound ref.
func TestPushStep_PublishesToBoundPushBranch(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, submittedHead := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, submittedHead, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.PushBranch = strptr("stacked-pr")
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, submittedHead)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("push step failed: %v", err)
	}

	remoteHead := gitCmd(t, upstream, "rev-parse", "refs/heads/stacked-pr")
	if remoteHead != submittedHead {
		t.Fatalf("bound remote head = %s, want %s", remoteHead, submittedHead)
	}
	if out, err := exec.Command("git", "-C", upstream, "rev-parse", "refs/heads/feature").CombinedOutput(); err == nil {
		t.Fatalf("remote got the local branch ref %s; the run must publish only its bound ref", strings.TrimSpace(string(out)))
	}
	gateHead := gitCmd(t, sctx.GateDir, "rev-parse", "refs/heads/feature")
	if gateHead != submittedHead {
		t.Fatalf("gate mirror ref = %s, want %s", gateHead, submittedHead)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PushRef == nil || *run.PushRef != "refs/heads/stacked-pr" {
		t.Fatalf("recorded push ref = %v, want refs/heads/stacked-pr", run.PushRef)
	}
}

// boundPRTestHost fakes the SCM surface resolveBoundPR reads.
type boundPRTestHost struct {
	scm.Host
	findCalls int
	findPR    *scm.PR
	findErr   error
	states    map[string]scm.PRState
	stateErr  error
	base      string
	baseErr   error
}

func (h *boundPRTestHost) FindPR(context.Context, string, string) (*scm.PR, error) {
	h.findCalls++
	return h.findPR, h.findErr
}

func (h *boundPRTestHost) GetPRState(_ context.Context, pr *scm.PR) (scm.PRState, error) {
	if h.stateErr != nil {
		return "", h.stateErr
	}
	if h.states != nil {
		if state, ok := h.states[pr.URL]; ok {
			return state, nil
		}
	}
	return scm.PRStateOpen, nil
}

func (h *boundPRTestHost) GetPRBaseBranch(context.Context, *scm.PR) (string, error) {
	return h.base, h.baseErr
}

func TestResolveBoundPR(t *testing.T) {
	t.Parallel()
	owned := "https://github.com/test/repo/pull/2020"
	other := &scm.PR{Number: "99", URL: "https://github.com/test/repo/pull/99"}

	t.Run("no owned URL discovers by branch", func(t *testing.T) {
		host := &boundPRTestHost{findPR: other}
		sctx := &pipeline.StepContext{Run: &db.Run{Branch: "refs/heads/feature"}}
		got, err := resolveBoundPR(context.Background(), sctx, host, "feature")
		if err != nil || got != other {
			t.Fatalf("resolveBoundPR = %+v, %v; want discovered %+v", got, err, other)
		}
		if host.findCalls != 1 {
			t.Fatalf("FindPR calls = %d, want 1", host.findCalls)
		}
	})

	t.Run("open owned PR binds without discovery", func(t *testing.T) {
		// The point of owned-first ordering: a second unrelated open PR on
		// the same head cannot trip the multi-head refusal, because FindPR
		// never runs.
		host := &boundPRTestHost{findErr: fmt.Errorf("listing would refuse on duplicate heads"), base: "main"}
		sctx := &pipeline.StepContext{Run: &db.Run{Branch: "refs/heads/feature", PRURL: strptr(owned)}}
		got, err := resolveBoundPR(context.Background(), sctx, host, "feature")
		if err != nil {
			t.Fatalf("resolveBoundPR error = %v", err)
		}
		if host.findCalls != 0 {
			t.Fatalf("FindPR ran %d times for an owned open PR", host.findCalls)
		}
		if got == nil || got.URL != owned {
			t.Fatalf("resolveBoundPR = %+v, want owned %s", got, owned)
		}
		if got.BaseBranch != "main" {
			t.Fatalf("owned PR base filled = %q, want main", got.BaseBranch)
		}
	})

	t.Run("stale owned PR refuses with a per-run base", func(t *testing.T) {
		host := &boundPRTestHost{
			states: map[string]scm.PRState{owned: scm.PRStateClosed},
			findPR: other,
		}
		sctx := &pipeline.StepContext{Run: &db.Run{Branch: "refs/heads/feature", PRURL: strptr(owned), PRBaseBranch: strptr("develop")}}
		got, err := resolveBoundPR(context.Background(), sctx, host, "feature")
		if err == nil {
			t.Fatalf("resolveBoundPR = %+v, want stale-refusal error", got)
		}
		if !strings.Contains(err.Error(), "stale") {
			t.Fatalf("error = %v, want stale persisted PR refusal", err)
		}
	})

	t.Run("stale owned PR falls back to discovery", func(t *testing.T) {
		host := &boundPRTestHost{
			states: map[string]scm.PRState{owned: scm.PRStateMerged},
			findPR: other,
		}
		sctx := &pipeline.StepContext{Run: &db.Run{Branch: "refs/heads/feature", PRURL: strptr(owned)}}
		got, err := resolveBoundPR(context.Background(), sctx, host, "feature")
		if err != nil || got != other {
			t.Fatalf("resolveBoundPR = %+v, %v; want discovered %+v", got, err, other)
		}
	})

	t.Run("unreadable owned PR fails closed", func(t *testing.T) {
		host := &boundPRTestHost{stateErr: fmt.Errorf("gh pr view failed")}
		sctx := &pipeline.StepContext{Run: &db.Run{Branch: "refs/heads/feature", PRURL: strptr(owned)}}
		if _, err := resolveBoundPR(context.Background(), sctx, host, "feature"); err == nil {
			t.Fatal("expected unreadable owned PR state to error")
		}
	})

	boundRun := func() *db.Run {
		return &db.Run{Branch: "refs/heads/feature", PushBranch: strptr("stacked-pr"), PRURL: strptr(owned)}
	}
	ownedPR := &scm.PR{Number: "2020", URL: owned, HeadBranch: "stacked-pr", BaseBranch: "main"}

	t.Run("bound run keeps the owned PR whose head is the publish branch", func(t *testing.T) {
		host := &boundPRTestHost{findPR: ownedPR, base: "main"}
		sctx := &pipeline.StepContext{Run: boundRun()}
		got, err := resolveBoundPR(context.Background(), sctx, host, "stacked-pr")
		if err != nil || got == nil || got.URL != owned {
			t.Fatalf("resolveBoundPR = %+v, %v; want owned %s", got, err, owned)
		}
	})

	t.Run("bound run refuses an owned PR when the publish branch has another", func(t *testing.T) {
		// `axi rerun --push-branch` re-bound the run: the persisted URL now
		// names the previous head's review object, and taking it would update
		// that PR while pushing to a different branch.
		host := &boundPRTestHost{findPR: other, base: "main"}
		sctx := &pipeline.StepContext{Run: boundRun()}
		_, err := resolveBoundPR(context.Background(), sctx, host, "stacked-pr")
		if err == nil {
			t.Fatal("expected refusal when the bound publish branch has a different PR")
		}
		if !strings.Contains(err.Error(), "is not this run's persisted") {
			t.Fatalf("error = %v, want a persisted-identity refusal", err)
		}
	})

	t.Run("bound run refuses when the publish branch has no PR of its own", func(t *testing.T) {
		host := &boundPRTestHost{base: "main"}
		sctx := &pipeline.StepContext{Run: boundRun()}
		_, err := resolveBoundPR(context.Background(), sctx, host, "stacked-pr")
		if err == nil {
			t.Fatal("expected refusal when the bound publish branch has no PR")
		}
		if !strings.Contains(err.Error(), "no pull request of its own") {
			t.Fatalf("error = %v, want a missing-head refusal", err)
		}
	})

	t.Run("bound run propagates discovery errors on an owned PR", func(t *testing.T) {
		host := &boundPRTestHost{findErr: fmt.Errorf("forge unreachable"), base: "main"}
		sctx := &pipeline.StepContext{Run: boundRun()}
		if _, err := resolveBoundPR(context.Background(), sctx, host, "stacked-pr"); err == nil {
			t.Fatal("expected the head verification failure to fail closed")
		}
	})
}

// openPRListTestHost is a scm.Host carrying the optional OpenPRLister
// interface so refuseAmbiguousCreateTarget can be driven without a forge.
type openPRListTestHost struct {
	scm.Host
	prs []scm.PR
	err error
}

func (h *openPRListTestHost) ListOpenPRs(context.Context) ([]scm.PR, error) {
	return h.prs, h.err
}

func TestRefuseAmbiguousCreateTarget(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	newSctx := func() *pipeline.StepContext {
		return newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	}

	candidate := func(headBranch, headSHA, base string) scm.PR {
		return scm.PR{Number: "2020", URL: "https://github.com/test/repo/pull/2020", HeadBranch: headBranch, HeadSHA: headSHA, BaseBranch: base}
	}

	t.Run("host without lister keeps prior behavior", func(t *testing.T) {
		if err := refuseAmbiguousCreateTarget(context.Background(), newSctx(), &readerlessHost{}, "feature", "main"); err != nil {
			t.Fatalf("error = %v, want nil for host without OpenPRLister", err)
		}
	})

	t.Run("listing failure refuses closed", func(t *testing.T) {
		host := &openPRListTestHost{err: fmt.Errorf("forge unreachable")}
		if err := refuseAmbiguousCreateTarget(context.Background(), newSctx(), host, "feature", "main"); err == nil {
			t.Fatal("expected refusal when the open-PR set cannot be read")
		}
	})

	t.Run("stacked PR head inside proposed head refuses", func(t *testing.T) {
		// Issue #552: baseSHA is an ancestor of HEAD and is the recorded head
		// of an open PR on another branch.
		host := &openPRListTestHost{prs: []scm.PR{candidate("stacked-pr", baseSHA, "main")}}
		err := refuseAmbiguousCreateTarget(context.Background(), newSctx(), host, "feature", "main")
		if err == nil {
			t.Fatal("expected refusal for a create duplicating an open PR's content")
		}
		for _, remedy := range []string{"--push-branch stacked-pr", "--base-branch stacked-pr", "--skip pr"} {
			if !strings.Contains(err.Error(), remedy) {
				t.Fatalf("refusal must name the %q remedy, got: %v", remedy, err)
			}
		}
	})

	t.Run("legitimate stack onto the candidate branch is allowed", func(t *testing.T) {
		// base == the candidate's head branch: a real stacked PR whose diff
		// is only the new work.
		host := &openPRListTestHost{prs: []scm.PR{candidate("stacked-pr", baseSHA, "main")}}
		if err := refuseAmbiguousCreateTarget(context.Background(), newSctx(), host, "feature", "stacked-pr"); err != nil {
			t.Fatalf("stacked PR onto the candidate branch refused: %v", err)
		}
	})

	t.Run("same-head candidate cannot exist at create", func(t *testing.T) {
		host := &openPRListTestHost{prs: []scm.PR{candidate("feature", baseSHA, "main")}}
		if err := refuseAmbiguousCreateTarget(context.Background(), newSctx(), host, "feature", "main"); err != nil {
			t.Fatalf("same-branch listing row must be skipped: %v", err)
		}
	})

	t.Run("non-ancestor candidate head is skipped", func(t *testing.T) {
		gitCmd(t, dir, "checkout", "-b", "other-line", baseSHA)
		if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", "other line commit")
		otherHead := gitCmd(t, dir, "rev-parse", "HEAD")
		gitCmd(t, dir, "checkout", "feature")

		host := &openPRListTestHost{prs: []scm.PR{candidate("other-line", otherHead, "main")}}
		if err := refuseAmbiguousCreateTarget(context.Background(), newSctx(), host, "feature", "main"); err != nil {
			t.Fatalf("non-ancestor candidate must be skipped: %v", err)
		}
	})

	t.Run("fork-only PR head resolves through the review ref", func(t *testing.T) {
		// A PR whose head lives only on a fork has no upstream branch; the
		// check verifies it through refs/pull/<n>/head on the base repo.
		gitCmd(t, dir, "checkout", "-b", "fork-line", baseSHA)
		if err := os.WriteFile(filepath.Join(dir, "fork.txt"), []byte("fork\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", "fork line commit")
		forkHead := gitCmd(t, dir, "rev-parse", "HEAD")
		gitCmd(t, dir, "checkout", "feature")
		gitCmd(t, dir, "update-ref", "refs/pull/4242/head", forkHead)

		host := &openPRListTestHost{prs: []scm.PR{
			{Number: "4242", URL: "https://github.com/test/repo/pull/4242", HeadBranch: "someone:only-branch", HeadSHA: forkHead, BaseBranch: "main"},
		}}
		if err := refuseAmbiguousCreateTarget(context.Background(), newSctx(), host, "feature", "main"); err != nil {
			t.Fatalf("verified non-ancestor fork PR must not block create: %v", err)
		}
	})

	t.Run("fork duplicate detected through the review ref live tip", func(t *testing.T) {
		// The recorded head is stale or unverifiable, but the live review-ref
		// tip is contained in the proposed head: still a duplicate.
		gitCmd(t, dir, "update-ref", "refs/pull/5555/head", baseSHA)
		host := &openPRListTestHost{prs: []scm.PR{
			{Number: "5555", URL: "https://github.com/test/repo/pull/5555", HeadBranch: "someone:stacked-pr", HeadSHA: strings.Repeat("ff", 20), BaseBranch: "main"},
		}}
		err := refuseAmbiguousCreateTarget(context.Background(), newSctx(), host, "feature", "main")
		if err == nil {
			t.Fatal("expected refusal when the review ref tip is contained in the proposed head")
		}
		if !strings.Contains(err.Error(), "refusing to create pull request") {
			t.Fatalf("error = %v, want a duplicate refusal", err)
		}
	})

	t.Run("same-named upstream branch does not attest a fork PR", func(t *testing.T) {
		// A fork PR's head can share a name with an upstream branch it has no
		// relation to. The fallback fetch must never read that branch's tip as
		// the PR's live head - the refusal is a verification failure, not a
		// duplicate claim.
		gitCmd(t, dir, "branch", "-f", "upstream-lookalike", baseSHA)
		host := &openPRListTestHost{prs: []scm.PR{
			{Number: "7777", URL: "https://github.com/test/repo/pull/7777", HeadBranch: "upstream-lookalike", HeadSHA: strings.Repeat("ee", 20), BaseBranch: "main"},
		}}
		err := refuseAmbiguousCreateTarget(context.Background(), newSctx(), host, "feature", "main")
		if err == nil {
			t.Fatal("expected a verification refusal for an unverifiable recorded head")
		}
		if strings.Contains(err.Error(), "refusing to create pull request") {
			t.Fatalf("an unrelated upstream branch must not pose as the PR's live head: %v", err)
		}
		if !strings.Contains(err.Error(), "could not verify") {
			t.Fatalf("error = %v, want a verification refusal", err)
		}
	})

	t.Run("unverifiable candidate branch refuses", func(t *testing.T) {
		missing := strings.Repeat("ab", 20)
		host := &openPRListTestHost{prs: []scm.PR{candidate("ghost-branch", missing, "main")}}
		err := refuseAmbiguousCreateTarget(context.Background(), newSctx(), host, "feature", "main")
		if err == nil {
			t.Fatal("expected refusal when an open PR's head cannot be verified")
		}
		if !strings.Contains(err.Error(), "could not verify") {
			t.Fatalf("refusal must identify the unverifiable candidate, got: %v", err)
		}
	})
}

// TestPRStep_BindsByPushBranch asserts the head FindPR is asked for is the
// bound publish branch, not the run's local branch name.
func TestPRStep_BindsByPushBranch(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PushBranch = strptr("stacked-pr")
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.PRURL != "https://github.com/test/repo/pull/42" {
		t.Fatalf("PRURL = %q, want the bound PR", outcome.PRURL)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	ghLog := string(logData)
	if !strings.Contains(ghLog, "pr list --head stacked-pr") {
		t.Fatalf("PR lookup must query the bound publish branch, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "pr list --head feature") {
		t.Fatalf("PR lookup used the local branch name, got:\n%s", ghLog)
	}
}

// TestPRStep_SameHeadMultiplicityRefuses covers the FindPR guard: two open
// PRs on the same source branch can never be told apart, so the step fails
// closed instead of inheriting whichever listing order surfaced.
func TestPRStep_SameHeadMultiplicityRefuses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")
	env = append(env, `FAKE_CLI_PR_LIST_JSON=[{"number":1,"url":"https://github.com/test/repo/pull/1","baseRefName":"main"},{"number":2,"url":"https://github.com/test/repo/pull/2","baseRefName":"main"}]`)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	_, err := (&PRStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected same-head multiplicity to fail closed")
	}
	if !strings.Contains(err.Error(), "share head branch") {
		t.Fatalf("error = %v, want ambiguous-head refusal", err)
	}
	logData, readErr := os.ReadFile(logFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "pr create") || strings.Contains(string(logData), "pr edit") {
		t.Fatalf("ambiguous listing must stop before PR mutation, got:\n%s", logData)
	}
}

func TestLastKnownBranchTip_MatchesBoundPublishBranch(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})

	pushed := strings.Repeat("ab", 20)
	if _, err := sctx.DB.InsertRepoWithID("repo-1", dir, "https://github.com/test/repo", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertRunWithIntent("repo-1", "refs/heads/feature", headSHA, baseSHA, nil, ""); err != nil {
		t.Fatal(err)
	}
	// A prior run that published onto stacked-pr via --push-branch is the
	// right lease anchor only for pushes targeting that same remote branch.
	runBound, err := sctx.DB.InsertRunWithIntentAndLaunchNonce("repo-1", "refs/heads/local-name", headSHA, baseSHA, nil, "", "", "", "", "stacked-pr", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunPublication(runBound.ID, db.PushBinding{HeadSHA: pushed, Ref: "refs/heads/stacked-pr"}); err != nil {
		t.Fatal(err)
	}

	if got := lastKnownBranchTip(context.Background(), sctx, "stacked-pr", false); got != pushed {
		t.Fatalf("lastKnownBranchTip(stacked-pr) = %q, want prior bound publication %q", got, pushed)
	}
	if got := lastKnownBranchTip(context.Background(), sctx, "local-name", false); got == pushed {
		t.Fatalf("a --push-branch run's publication must not lease the local branch name, got %q", got)
	}
}
