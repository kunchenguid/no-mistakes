package citest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// pushDescendantRewrite advances the published branch to a commit the run
// produced nowhere, the way a native stack rebase or a partial stack merge
// does. The commit descends from the run's head, so it is attributable
// content - only its validation is missing.
func pushDescendantRewrite(t *testing.T, dir string) string {
	t.Helper()
	parent := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
	tree := stepstest.GitCmd(t, dir, "rev-parse", "HEAD^{tree}")
	rewritten := stepstest.GitCmd(t, dir, "commit-tree", tree, "-p", parent, "-m", "rewritten outside the run")
	stepstest.GitCmd(t, dir, "push", "origin", rewritten+":refs/heads/feature")
	return rewritten
}

// pushOrphanRewrite replaces the published branch with an unrelated root
// commit, so nothing in the run can account for it.
func pushOrphanRewrite(t *testing.T, dir string) string {
	t.Helper()
	tree := stepstest.GitCmd(t, dir, "rev-parse", "HEAD^{tree}")
	orphan := stepstest.GitCmd(t, dir, "commit-tree", tree, "-m", "stray root commit")
	stepstest.GitCmd(t, dir, "push", "origin", "--force", orphan+":refs/heads/feature")
	return orphan
}

func revalidateEnv(t *testing.T, checksJSON, prBase string) []string {
	env := stepstest.FakeCIGH(t, "OPEN", checksJSON)
	if prBase != "" {
		env = append(env, "FAKE_CLI_PR_BASE="+prBase)
	}
	return env
}

func revalidateContext(t *testing.T, dir, upstream, baseSHA, headSHA string, env []string) (*stepstest.MockAgent, *pipeline.StepContext, *[]string) {
	t.Helper()
	prURL := "https://github.com/test/repo/pull/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Second
	logs := &[]string{}
	sctx.Log = func(s string) { *logs = append(*logs, s) }
	return ag, sctx, logs
}

// A head rewritten outside the run is adopted: the run fetches and re-verifies
// it from the push target, moves onto it through the same durable path a CI
// repair uses, aligns the run's base with the pull request's live base, and
// restarts at Review so the rewritten content is revalidated.
func TestCIStep_AdoptsAPublishedHeadRewriteAndRestartsAtReview(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	rewritten := pushDescendantRewrite(t, dir)

	env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "develop")
	_, sctx, logs := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	outcome, err := (&steps.CIStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("adoption returned error: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want a restart outcome", outcome)
	}
	if outcome.RestartFrom != types.StepReview {
		t.Fatalf("RestartFrom = %q, want review", outcome.RestartFrom)
	}
	if err := (&steps.CIStep{}).PrepareRestart(sctx, outcome.RestartFrom); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(*logs, "\n")
	for _, want := range []string{
		"pull request branch head moved outside the run (",
		"adopted rewritten pull request head " + short(rewritten),
		`run base branch updated to the pull request's live base "develop" (was "main")`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("log is missing %q:\n%s", want, joined)
		}
	}
	if sctx.Run.HeadSHA != rewritten {
		t.Fatalf("in-memory run head = %s, want the adopted %s", sctx.Run.HeadSHA, rewritten)
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != rewritten {
		t.Fatalf("worktree head = %s, want the adopted %s", got, rewritten)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != rewritten {
		t.Fatalf("durable run head = %s, want the adopted %s", run.HeadSHA, rewritten)
	}
	if run.ReviewApprovedHeadSHA != nil {
		t.Fatalf("review approval = %q, want cleared for revalidation", *run.ReviewApprovedHeadSHA)
	}
	if run.PRBaseBranch == nil || *run.PRBaseBranch != "develop" {
		t.Fatalf("persisted base = %v, want the pull request's live base develop", run.PRBaseBranch)
	}
}

// A head the run cannot account for - an unrelated history nobody in the run
// produced - parks as ask-user instead of being adopted. Nothing is mutated:
// the recorded head, the worktree, and the remote stay exactly as they were.
func TestCIStep_ParksOnAPublishedHeadRewriteItCannotAttribute(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	orphan := pushOrphanRewrite(t, dir)

	env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "")
	_, sctx, logs := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	outcome, err := (&steps.CIStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("park returned error: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want an ask-user park", outcome)
	}
	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("decode findings: %v", err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryCIHeadRewrite ||
		findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("findings = %+v, want one ci-head-rewrite ask-user finding", findings.Items)
	}
	if !strings.Contains(findings.Items[0].Description, "neither on the live head nor recorded as published") {
		t.Fatalf("finding = %q, want the attribution refusal", findings.Items[0].Description)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "not adopting "+short(orphan)) {
		t.Fatalf("log is missing the refusal:\n%s", strings.Join(*logs, "\n"))
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("worktree head moved to %s", got)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != headSHA {
		t.Fatalf("durable run head = %s, want it untouched", run.HeadSHA)
	}
}

// Adopting must never discard uncommitted work: a dirty run worktree parks
// for an operator decision even when the live head is otherwise attributable.
func TestCIStep_ParksOnAPublishedHeadRewriteWithADirtyWorktree(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	rewritten := pushDescendantRewrite(t, dir)
	os.WriteFile(filepath.Join(dir, "uncommitted.txt"), []byte("wip"), 0o644)

	env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "")
	_, sctx, logs := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	outcome, err := (&steps.CIStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("park returned error: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want an ask-user park", outcome)
	}
	if !strings.Contains(outcome.Findings, "would discard uncommitted work") {
		t.Fatalf("findings = %q, want the dirty-worktree refusal", outcome.Findings)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "not adopting "+short(rewritten)) {
		t.Fatalf("log is missing the refusal:\n%s", strings.Join(*logs, "\n"))
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("worktree head moved to %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "uncommitted.txt")); err != nil {
		t.Fatalf("uncommitted work was discarded: %v", err)
	}
}

// The checks read is the second observer of the pull request's live head: a
// rewrite that lands between the branch-head read and the checks read still
// cannot report checks for a head the run never validated. With the push target
// unreadable nothing can establish ownership, so the poll reads no check at all
// and the monitor waits and reads again, rather than failing the whole run over
// a transient ls-remote blip.
func TestCIStep_ChecksReadWaitsRatherThanReportingChecksForAForeignHead(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	// A head that exists only locally: the provider reports it, but the push
	// target no longer serves the branch at all.
	tree := stepstest.GitCmd(t, dir, "rev-parse", "HEAD^{tree}")
	ghost := stepstest.GitCmd(t, dir, "commit-tree", tree, "-m", "ghost rewrite")
	stepstest.GitCmd(t, dir, "tag", "nm-ghost", ghost)
	stepstest.GitCmd(t, dir, "push", "origin", "--delete", "feature")

	env := append(revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, ""), "FAKE_CLI_PR_HEAD_SHA="+ghost)
	_, sctx, logs := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		polls++
		if polls >= 2 {
			return errors.New("stop polling")
		}
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "stop polling") {
		t.Fatalf("outcome = %#v, err = %v, want the monitor to have kept polling", outcome, err)
	}
	joined := strings.Join(*logs, "\n")
	for _, want := range []string{"warning: could not read the published branch head", "no check result is read this poll"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("log is missing %q:\n%s", want, joined)
		}
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("worktree head moved to %s", got)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != headSHA {
		t.Fatalf("durable run head = %s, want it untouched", run.HeadSHA)
	}
}

// An unreadable push target on its own only warns: the monitor keeps polling
// and the next poll reads the branch head again, instead of adopting or
// failing off one failed read.
func TestCIStep_UnreadablePublishedHeadOnlyWarns(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	stepstest.GitCmd(t, dir, "push", "origin", "--delete", "feature")

	env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "")
	_, sctx, logs := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	ctx, cancel := context.WithCancel(context.Background())
	sctx.Ctx = ctx
	t.Cleanup(cancel)
	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		polls++
		if polls >= 3 {
			return errors.New("stop polling")
		}
		return nil
	})
	_, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "stop polling") {
		t.Fatalf("error = %v, want the monitor to keep polling", err)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "warning: could not read the published branch head") {
		t.Fatalf("log is missing the unreadable-target warning:\n%s", strings.Join(*logs, "\n"))
	}
	if polls < 3 {
		t.Fatalf("polls = %d, want the monitor to have kept polling", polls)
	}
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// The provider's live head and the push target are two different reads. When
// the provider names a foreign head but the push target still serves a head
// this run owns, there is nothing to revalidate: the monitor adopts nothing,
// restarts nothing, and keeps polling instead of cycling Review forever.
func TestCIStep_ForeignProviderHeadWithAnOwnedPushTargetAdoptsNothing(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)

	tree := stepstest.GitCmd(t, dir, "rev-parse", "HEAD^{tree}")
	ghost := stepstest.GitCmd(t, dir, "commit-tree", tree, "-m", "head only the provider reports")

	env := append(revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, ""), "FAKE_CLI_PR_HEAD_SHA="+ghost)
	_, sctx, logs := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		polls++
		if polls >= 2 {
			return errors.New("stop polling")
		}
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "stop polling") {
		t.Fatalf("outcome = %#v, err = %v, want the monitor to have kept polling", outcome, err)
	}
	// The stall is what eventually parks on the generic CI timeout, so each
	// non-adopting poll has to name both heads to be diagnosable.
	joined := strings.Join(*logs, "\n")
	for _, want := range []string{"not adopting anything this poll", "reports head " + short(ghost), "push target serves " + short(headSHA)} {
		if !strings.Contains(joined, want) {
			t.Fatalf("log is missing %q:\n%s", want, joined)
		}
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("worktree head moved to %s", got)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != headSHA {
		t.Fatalf("durable run head = %s, want it untouched", run.HeadSHA)
	}
	if run.ReviewApprovedHeadSHA != nil && *run.ReviewApprovedHeadSHA != headSHA {
		t.Fatalf("review approval = %q, want it untouched", *run.ReviewApprovedHeadSHA)
	}
}

// A pull request whose live base the forge will not name fails the step
// closed, and closed means nothing moved: the live base is read before the
// adoption touches the worktree, the run head, or the review approval, so the
// run is still sitting on the head it validated when the step gives up.
func TestCIStep_AdoptionFailsClosedWhenTheLiveBaseIsUnreported(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	rewritten := pushDescendantRewrite(t, dir)

	env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "-")
	_, sctx, _ := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)
	approved := headSHA
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, approved); err != nil {
		t.Fatal(err)
	}

	outcome, err := (&steps.CIStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "no live base branch") {
		t.Fatalf("outcome = %#v, err = %v, want the base alignment to fail closed", outcome, err)
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("worktree head = %s, want the validated %s (the rewrite %s must not be adopted)", got, headSHA, rewritten)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRBaseBranch != nil {
		t.Fatalf("persisted base = %q, want no base recorded", *run.PRBaseBranch)
	}
	if run.HeadSHA != headSHA {
		t.Fatalf("durable run head = %s, want the validated %s", run.HeadSHA, headSHA)
	}
	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != approved {
		t.Fatalf("review approval = %v, want it untouched at %s", run.ReviewApprovedHeadSHA, approved)
	}
}

// The forge-reported base is durable and is re-read as a ref name by every
// later step, so a name Git cannot use is refused before anything is mutated -
// the same validation every operator-facing writer of that field runs.
func TestCIStep_AdoptionRefusesAnUnusableLiveBaseName(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	rewritten := pushDescendantRewrite(t, dir)

	env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "bad..base")
	_, sctx, _ := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	outcome, err := (&steps.CIStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "not a usable branch name") {
		t.Fatalf("outcome = %#v, err = %v, want the unusable base refused", outcome, err)
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("worktree head = %s, want the validated %s (the rewrite %s must not be adopted)", got, headSHA, rewritten)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRBaseBranch != nil {
		t.Fatalf("persisted base = %q, want no base recorded", *run.PRBaseBranch)
	}
	if run.HeadSHA != headSHA {
		t.Fatalf("durable run head = %s, want the validated %s", run.HeadSHA, headSHA)
	}
}

// A finding a human left unselected at an earlier CI gate is the operator's
// outstanding decision, and every other terminal CI park and restart carries it
// forward. The head-rewrite park and the adoption restart must too: dropping it
// loses the decision with no record, since nothing on these paths clears it.
func TestCIStep_HeadRewriteOutcomesCarryTheDeferredFindings(t *testing.T) {
	t.Parallel()

	const deferred = `{"findings":[{"id":"ci-2","severity":"warning","description":"CI check failing: lint - left for a human","action":"ask-user","category":"ci-check","check":"lint"}],"summary":"1 unselected CI finding"}`

	t.Run("park", func(t *testing.T) {
		t.Parallel()
		dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
		pushOrphanRewrite(t, dir)

		env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "")
		_, sctx, _ := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)
		sctx.DeferredFindings = deferred

		outcome, err := (&steps.CIStep{}).Execute(sctx)
		if err != nil {
			t.Fatalf("park returned error: %v", err)
		}
		if outcome == nil || !outcome.NeedsApproval {
			t.Fatalf("outcome = %#v, want an ask-user park", outcome)
		}
		var findings types.Findings
		if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
			t.Fatalf("decode findings: %v", err)
		}
		var sawRewrite, sawDeferred bool
		for _, item := range findings.Items {
			if item.Category == types.FindingCategoryCIHeadRewrite {
				sawRewrite = true
			}
			if item.ID == "ci-2" {
				sawDeferred = true
			}
		}
		if !sawRewrite || !sawDeferred {
			t.Fatalf("findings = %+v, want both the head-rewrite park and the operator's unselected finding", findings.Items)
		}
	})

	t.Run("adoption restart", func(t *testing.T) {
		t.Parallel()
		dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
		pushDescendantRewrite(t, dir)

		env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "develop")
		_, sctx, _ := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)
		sctx.DeferredFindings = deferred

		outcome, err := (&steps.CIStep{}).Execute(sctx)
		if err != nil {
			t.Fatalf("adoption returned error: %v", err)
		}
		if outcome == nil || outcome.RestartFrom != types.StepReview {
			t.Fatalf("outcome = %#v, want a restart from Review", outcome)
		}
		var findings types.Findings
		if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
			t.Fatalf("decode findings: %v", err)
		}
		if len(findings.Items) != 1 || findings.Items[0].ID != "ci-2" {
			t.Fatalf("findings = %+v, want the operator's unselected finding carried on the restart", findings.Items)
		}
	})
}

// The commits between the recorded head and an adopted head were written
// outside the run by the rewrite that moved the branch, so the adoption must
// not persist them as an uncertified PIPELINE range: Review would then be told
// they were "authored by a previous run's fixer" and offered the
// revert-to-minimal-fix ramp over the author's own work.
func TestCIStep_AdoptionDoesNotClaimTheForeignCommitsAsPipelineAuthored(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	rewritten := pushDescendantRewrite(t, dir)

	env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "develop")
	_, sctx, _ := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	outcome, err := (&steps.CIStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("adoption returned error: %v", err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("outcome = %#v, want a restart from Review", outcome)
	}
	if sctx.Run.HeadSHA != rewritten {
		t.Fatalf("run head = %s, want the adopted %s", sctx.Run.HeadSHA, rewritten)
	}
	rng, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if rng != nil {
		t.Fatalf("uncertified pipeline range = %+v, want none: the adopted commits are not the pipeline's", rng)
	}
}

// A live-base read the forge does not answer says nothing about the head, so it
// is treated like every other failed read in the adoption: warn and let the next
// poll try again. Failing the run there abandoned a run that had not mutated
// anything yet over a transport blip. An empty-but-successful base and an
// unusable branch name stay hard failures - those are permanent.
func TestCIStep_AdoptionWaitsWhenTheLiveBaseReadFails(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	rewritten := pushDescendantRewrite(t, dir)

	env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "!")
	_, sctx, logs := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		polls++
		if polls >= 2 {
			return errors.New("stop polling")
		}
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "stop polling") {
		t.Fatalf("outcome = %#v, err = %v, want the monitor to have kept polling", outcome, err)
	}
	joined := strings.Join(*logs, "\n")
	for _, want := range []string{"not adopting " + short(rewritten), "this poll", "live base branch could not be read"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("log is missing %q:\n%s", want, joined)
		}
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("worktree head = %s, want the validated %s untouched", got, headSHA)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != headSHA || run.PRBaseBranch != nil {
		t.Fatalf("run = {head %s, base %v}, want both untouched", run.HeadSHA, run.PRBaseBranch)
	}
}

// The ownership guard belongs to the poll, not to one PR-state branch: a
// transient PR-state read failure used to skip it entirely - and the second
// guard only sees a live head on GitHub - so the foreign head's checks reached
// the findings and the auto_fix.ci loop for commits the run never validated.
// The guard now runs whatever the state read did, but it only SKIPS there: the
// merged and closed arms end a run, so adopting under an unreadable state would
// reset the worktree and restart Review on a pull request that may already be
// merged. The next poll with a readable state adopts.
func TestCIStep_PRStateUnknownSkipsChecksWithoutAdopting(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	rewritten := pushDescendantRewrite(t, dir)

	env := append(revalidateEnv(t, `[{"name":"test","state":"FAILURE","bucket":"fail"}]`, "develop"),
		"FAKE_CLI_STATE_ERR=the pull request state could not be read")
	_, sctx, logs := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		polls++
		if polls >= 2 {
			return errors.New("stop polling")
		}
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "stop polling") {
		t.Fatalf("outcome = %#v, err = %v, want the monitor to have kept polling", outcome, err)
	}
	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, "the pull request state could not be read") {
		t.Fatalf("log does not name the unreadable state:\n%s", joined)
	}
	if strings.Contains(joined, "adopting the live head") {
		t.Fatalf("the monitor adopted under an unreadable pull request state:\n%s", joined)
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("worktree head = %s, want the reviewed %s (the rewrite %s must not be adopted yet)", got, headSHA, rewritten)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != headSHA || run.PRBaseBranch != nil {
		t.Fatalf("run = {head %s, base %v}, want both untouched", run.HeadSHA, run.PRBaseBranch)
	}
}

// A push target whose head cannot be read leaves ownership unproven, so no
// check is read - and without a bound that is an invisible spin: under an
// unlimited ci_timeout it never ends, and under a finite one it parks on the
// wrong condition. It follows the same rule the provider check read does: after
// the same number of consecutive failures it parks with a finding naming the
// unreadable push target.
func TestCIStep_UnreadablePublishedHeadParksAfterTheReadLimit(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	stepstest.GitCmd(t, dir, "push", "origin", "--delete", "feature")

	env := revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, "")
	_, sctx, _ := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error { return nil })
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("CI step returned error: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want an ask-user park once the read limit is hit", outcome)
	}
	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("decode findings: %v", err)
	}
	if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "push target") {
		t.Fatalf("findings = %+v, want one finding naming the unreadable push target", findings.Items)
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("worktree head moved to %s", got)
	}
}

// Every poll that ends without a check for a head this run owns is bounded by
// the same counter, because each of these conditions can repeat forever: under
// `ci_timeout: unlimited` the monitor never ends, and under a finite one it
// parks naming the wrong condition. Counting them separately left two of them
// unbounded - one never incremented anything, the other shared a counter the
// check read clears on every poll.
func TestCIStep_PollsThatReadNoOwnedCheckAreBounded(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(t *testing.T, dir string) []string{
		// The state read fails and the push target serves a head the run owns
		// nowhere: nothing may be adopted under an unknown state, and no check
		// may be read either.
		"state unreadable with a foreign head": func(t *testing.T, dir string) []string {
			pushDescendantRewrite(t, dir)
			return []string{"FAKE_CLI_STATE_ERR=the pull request state could not be read"}
		},
		// The provider keeps naming a head the push target does not serve, so
		// the two reads never agree and the adoption adopts nothing.
		"provider head the push target never serves": func(t *testing.T, dir string) []string {
			tree := stepstest.GitCmd(t, dir, "rev-parse", "HEAD^{tree}")
			ghost := stepstest.GitCmd(t, dir, "commit-tree", tree, "-m", "head only the provider reports")
			return []string{"FAKE_CLI_PR_HEAD_SHA=" + ghost}
		},
	} {
		setup := setup
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
			env := append(revalidateEnv(t, `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`, ""), setup(t, dir)...)
			_, sctx, _ := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

			step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error { return nil })
			outcome, err := step.Execute(sctx)
			if err != nil {
				t.Fatalf("CI step returned error: %v", err)
			}
			if outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("outcome = %#v, want an ask-user park once the unread-poll bound is hit", outcome)
			}
			var findings types.Findings
			if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
				t.Fatalf("decode findings: %v", err)
			}
			if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "a head this run validated") {
				t.Fatalf("findings = %+v, want one finding naming the unread condition", findings.Items)
			}
			// The decision this park asks for is which head to validate, so
			// the one predicate both automatic resolvers read must cover it:
			// without the category `axi --yes` and the TUI's yolo approved it,
			// and the approval then completed CI as a plain pass on a head no
			// Review certified.
			if !pipeline.HasCIHeadRewriteRefusal(outcome.Findings) {
				t.Fatalf("findings = %+v, want the head-rewrite category every automatic resolver stands aside on", findings.Items)
			}
			if findings.Items[0].ID == "" {
				t.Fatalf("findings = %+v, want a stable finding ID an operator can select", findings.Items)
			}
			if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Fatalf("worktree head moved to %s", got)
			}
		})
	}
}

// A merge observed at a head this run owns nowhere is a foreign merge. The
// provider proves exactly which head merged, so the step refuses it with the
// head-changed failure instead of recording a merged completion for a commit
// the run never validated - the outcome a server-side rewrite plus a fast
// stack merge would otherwise produce.
func TestCIStep_PRAtAForeignMergedHeadFailsInsteadOfCompleting(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	// A history nothing in the run can account for, reported as the merged
	// head by the provider.
	foreign := stepstest.GitCmd(t, dir, "commit-tree", stepstest.GitCmd(t, dir, "rev-parse", "HEAD^{tree}"), "-m", "merged from elsewhere")

	env := append(stepstest.FakeCIGH(t, "MERGED", "[]"), "FAKE_CLI_PR_HEAD_SHA="+foreign)
	_, sctx, logs := revalidateContext(t, dir, upstream, baseSHA, headSHA, env)

	outcome, err := (&steps.CIStep{}).Execute(sctx)
	if err == nil || !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("outcome = %#v, err = %v, want the foreign merged head refused", outcome, err)
	}
	if outcome != nil {
		t.Fatalf("outcome = %#v, want no completed outcome", outcome)
	}
	joined := strings.Join(*logs, "\n")
	if strings.Contains(joined, "PR has been merged!") {
		t.Fatalf("the run reported a foreign merged head as its own merge:\n%s", joined)
	}
	run, dbErr := sctx.DB.GetRun(sctx.Run.ID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if run.PRState != nil && *run.PRState == "merged" {
		t.Fatalf("run pr_state = %q, want no merged state recorded", *run.PRState)
	}
	if run.HeadSHA != headSHA {
		t.Fatalf("recorded head = %s, want the validated %s", run.HeadSHA, headSHA)
	}
}
