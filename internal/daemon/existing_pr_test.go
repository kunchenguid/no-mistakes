package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// stubExplicitUpstream stands a local bare repository in for the explicit PR's
// repository, so the launch's integration fetch resolves without a network.
func stubExplicitUpstream(t *testing.T, p *paths.Paths, repo *db.Repo, baseBranch string) {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, "", "init", "--bare", upstream)
	gitCmd(t, repo.WorkingPath, "push", upstream, "main:refs/heads/"+baseBranch)
	gitCmd(t, p.RepoDir(repo.ID), "config", "url."+upstream+".insteadOf", "https://github.com/upstream/widgets.git")
}

func TestExistingPRLaunchPinsTargetAndFailsClosed(t *testing.T) {
	bin := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, name), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake gh: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	const target = "https://github.com/upstream/widgets/pull/168"
	var calls atomic.Int32
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&existingPRObserveStep{calls: &calls, target: target}} })
	repo, _ := setupTestGitRepo(t, p, d, "explicit-pr")
	var err error
	repo, err = d.UpdateRepoForkURL(repo.ID, "https://github.com/contributor/widgets.git")
	if err != nil {
		t.Fatal(err)
	}
	stubExplicitUpstream(t, p, repo, "main")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "new.txt"), []byte("new committed work"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "new work")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	payload := fmt.Sprintf(`{"number":168,"html_url":%q,"state":"open","base":{"ref":"main","repo":{"full_name":"upstream/widgets","html_url":"https://github.com/upstream/widgets"}},"head":{"ref":"feature","sha":%q,"repo":{"full_name":"contributor/widgets","html_url":"https://github.com/contributor/widgets"}}}`, target, head)
	t.Setenv("FAKE_CLI_EXISTING_PR_JSON", payload)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := ipc.StartExistingPRRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "validate existing upstream PR", URL: target}
	var result ipc.RerunResult
	if err := client.Call(ipc.MethodStartExistingPRRun, &request, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted || run.ExistingPRURL == nil || *run.ExistingPRURL != target {
		t.Fatalf("run lost target: %+v", run)
	}
	if calls.Load() != 1 {
		t.Fatalf("step calls=%d", calls.Load())
	}
	if got := inheritablePRURL(run); got != "" {
		t.Fatalf("explicit URL leaked into legacy host discovery: %s", got)
	}
	legacy := *run
	legacy.ExistingPRURL = nil
	if got := inheritablePRURL(&legacy); got != target {
		t.Fatalf("legacy inheritance changed: %s", got)
	}
	// The launch binds the gate branch, so a run that never reached Push - the
	// ordinary outcome of a declined gate - still has a head rerun can resolve.
	if got := gitOutput(t, p.RepoDir(repo.ID), "rev-parse", "refs/heads/feature^{commit}"); got != head {
		t.Fatalf("gate branch = %s, want the submitted head %s", got, head)
	}
	// A rerun inherits the hard constraint, not just the legacy discovered URL.
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "feature", PreviousRunID: run.ID}, &rerun); err != nil {
		t.Fatal(err)
	}
	inherited := waitForRunTerminalState(t, d, rerun.RunID)
	if inherited.Status != types.RunCompleted || inherited.ExistingPRURL == nil || *inherited.ExistingPRURL != target {
		t.Fatalf("rerun lost target: %+v", inherited)
	}
	before := calls.Load()
	for _, tc := range []struct{ name, payload, head string }{
		{"wrong live head", strings.Replace(payload, head, strings.Repeat("f", 40), 1), head},
		{"wrong source", strings.Replace(payload, `"ref":"feature"`, `"ref":"other"`, 1), head},
		{"unreadable PR", "{}", head},
		{"wrong submitted head", payload, strings.Repeat("a", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_CLI_EXISTING_PR_JSON", tc.payload)
			req := request
			req.HeadSHA = tc.head
			if err := client.Call(ipc.MethodStartExistingPRRun, &req, &ipc.RerunResult{}); err == nil {
				t.Fatal("invalid explicit association started")
			}
			if calls.Load() != before {
				t.Fatal("invalid explicit association ran pipeline")
			}
		})
	}
	active, err := d.InsertRun(repo.ID, "feature", head, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ipc.MethodStartExistingPRRun, &request, &ipc.RerunResult{}); err == nil {
		t.Fatal("explicit launch replaced an unbound active run")
	}
	stillActive, err := d.GetRun(active.ID)
	if err != nil || stillActive.Status != types.RunPending {
		t.Fatalf("refused launch cancelled active run: %+v %v", stillActive, err)
	}
}

// The integration branch of an explicit run is the branch its pull request
// actually targets. Reading the repository default instead rebases, pushes and
// reports against another branch than the one CI merges into.
func TestExistingPRLaunchIntegratesWithTheValidatedBaseBranch(t *testing.T) {
	bin := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, name), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake gh: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	const target = "https://github.com/upstream/widgets/pull/168"
	const prBase = "release/2.0"
	var calls atomic.Int32
	observed := &existingPRObserveStep{calls: &calls, target: target, base: prBase}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{observed} })
	repo, _ := setupTestGitRepo(t, p, d, "explicit-pr-base")
	repo, err := d.UpdateRepoForkURL(repo.ID, "https://github.com/contributor/widgets.git")
	if err != nil {
		t.Fatal(err)
	}
	stubExplicitUpstream(t, p, repo, prBase)
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "new.txt"), []byte("new committed work"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "new work")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	payload := fmt.Sprintf(`{"number":168,"html_url":%q,"state":"open","base":{"ref":%q,"repo":{"full_name":"upstream/widgets","html_url":"https://github.com/upstream/widgets"}},"head":{"ref":"feature","sha":%q,"repo":{"full_name":"contributor/widgets","html_url":"https://github.com/contributor/widgets"}}}`, target, prBase, head)
	t.Setenv("FAKE_CLI_EXISTING_PR_JSON", payload)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RerunResult
	if err := client.Call(ipc.MethodStartExistingPRRun, &ipc.StartExistingPRRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "validate existing upstream PR", URL: target}, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted || run.PRBaseBranch == nil || *run.PRBaseBranch != prBase {
		t.Fatalf("run did not adopt the pull request base: %+v", run)
	}

	// A rerun reads the base back from the live pull request rather than
	// inheriting it as an operator override, which explicit runs refuse.
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "feature", PreviousRunID: run.ID}, &rerun); err != nil {
		t.Fatal(err)
	}
	inherited := waitForRunTerminalState(t, d, rerun.RunID)
	if inherited.Status != types.RunCompleted || inherited.PRBaseBranch == nil || *inherited.PRBaseBranch != prBase {
		t.Fatalf("rerun lost the pull request base: %+v", inherited)
	}
	if calls.Load() != 2 {
		t.Fatalf("step calls=%d", calls.Load())
	}
}

// The association is the branch's, not one run's: once an explicit launch
// proves it, later unflagged runs publish to that same pull request without
// discovering or creating another one - and they do so while carrying a NEW
// head, which is the whole point of the gate. It ends only when replaced by
// another explicit target or retired.
func TestExistingPRAssociationIsReusedUntilReplacedOrRetired(t *testing.T) {
	bin := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, name), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake gh: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	const target = "https://github.com/upstream/widgets/pull/168"
	const replacement = "https://github.com/upstream/widgets/pull/200"
	var calls atomic.Int32
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&existingPRObserveStep{calls: &calls, target: target}} })
	repo, _ := setupTestGitRepo(t, p, d, "explicit-pr-association")
	repo, err := d.UpdateRepoForkURL(repo.ID, "https://github.com/contributor/widgets.git")
	if err != nil {
		t.Fatal(err)
	}
	stubExplicitUpstream(t, p, repo, "main")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	writeCommit(t, repo.WorkingPath, "new.txt", "new committed work")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	prPayload := func(url, number, headSHA string) string {
		return fmt.Sprintf(`{"number":%s,"html_url":%q,"state":"open","base":{"ref":"main","repo":{"full_name":"upstream/widgets","html_url":"https://github.com/upstream/widgets"}},"head":{"ref":"feature","sha":%q,"repo":{"full_name":"contributor/widgets","html_url":"https://github.com/contributor/widgets"}}}`, number, url, headSHA)
	}
	t.Setenv("FAKE_CLI_EXISTING_PR_JSON", prPayload(target, "168", head))
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RerunResult
	if err := client.Call(ipc.MethodStartExistingPRRun, &ipc.StartExistingPRRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "validate existing upstream PR", URL: target}, &result); err != nil {
		t.Fatal(err)
	}
	if run := waitForRunTerminalState(t, d, result.RunID); run.Status != types.RunCompleted {
		t.Fatalf("explicit launch did not complete: %+v", run)
	}
	if stored, err := d.GetBranchPRTarget(repo.ID, "feature"); err != nil || stored != target {
		t.Fatalf("association = %q (%v), want %s", stored, err, target)
	}

	// The contributor commits again: the branch now has a head the pull
	// request has never seen, and an unflagged run must still publish to it.
	writeCommit(t, repo.WorkingPath, "more.txt", "later work")
	next := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	if next == head {
		t.Fatal("expected a new head")
	}
	gateDir := p.RepoDir(repo.ID)
	gitCmd(t, gateDir, "fetch", "--no-tags", "--", repo.WorkingPath, next)
	gitCmd(t, gateDir, "update-ref", "refs/heads/feature", next)
	var unflagged ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "feature", PreviousRunID: result.RunID}, &unflagged); err != nil {
		t.Fatal(err)
	}
	reused := waitForRunTerminalState(t, d, unflagged.RunID)
	if reused.Status != types.RunCompleted || reused.ExistingPRURL == nil || *reused.ExistingPRURL != target {
		t.Fatalf("unflagged run lost the association: %+v", reused)
	}
	if reused.HeadSHA != next {
		t.Fatalf("unflagged run head = %s, want the new head %s", reused.HeadSHA, next)
	}

	// A merged or closed target ends the association's usefulness, and every
	// later run on the branch fails until the operator acts - so the failure
	// has to say what the supported action is.
	t.Setenv("FAKE_CLI_EXISTING_PR_JSON", strings.Replace(prPayload(target, "168", next), `"state":"open"`, `"state":"closed"`, 1))
	err = client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "feature", PreviousRunID: unflagged.RunID}, &ipc.RerunResult{})
	if err == nil || !strings.Contains(err.Error(), "--retire-existing-pr") {
		t.Fatalf("stale association failure did not name the supported way out: %v", err)
	}
	t.Setenv("FAKE_CLI_EXISTING_PR_JSON", prPayload(target, "168", next))

	// Publishing to someone else's pull request means running the whole
	// pipeline, so a skip is refused at launch rather than failing at the step
	// that needed the published head.
	err = client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "feature", PreviousRunID: unflagged.RunID, SkipSteps: []types.StepName{types.StepPush}}, &ipc.RerunResult{})
	if err == nil || !strings.Contains(err.Error(), "cannot skip steps") {
		t.Fatalf("associated branch accepted a skip: %v", err)
	}

	// Retiring is a between-runs decision: an active run keeps publishing
	// where it was validated to publish.
	active, err := d.InsertRun(repo.ID, "feature", next, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ipc.MethodRetireExistingPR, &ipc.RetireExistingPRParams{RepoID: repo.ID, Branch: "feature"}, &ipc.RetireExistingPRResult{}); err == nil {
		t.Fatal("retired an association underneath an active run")
	}
	if err := d.UpdateRunStatus(active.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	// Replacement: a second explicit target takes over the branch.
	t.Setenv("FAKE_CLI_EXISTING_PR_JSON", prPayload(replacement, "200", next))
	var replaced ipc.RerunResult
	if err := client.Call(ipc.MethodStartExistingPRRun, &ipc.StartExistingPRRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: next, Intent: "move to the replacement PR", URL: replacement}, &replaced); err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, d, replaced.RunID)
	if stored, err := d.GetBranchPRTarget(repo.ID, "feature"); err != nil || stored != replacement {
		t.Fatalf("association after replacement = %q (%v), want %s", stored, err, replacement)
	}

	// Retirement returns the branch to ordinary discovery.
	var retired ipc.RetireExistingPRResult
	if err := client.Call(ipc.MethodRetireExistingPR, &ipc.RetireExistingPRParams{RepoID: repo.ID, Branch: "feature"}, &retired); err != nil {
		t.Fatal(err)
	}
	if retired.RetiredURL != replacement {
		t.Fatalf("retired %q, want %s", retired.RetiredURL, replacement)
	}
	if stored, err := d.GetBranchPRTarget(repo.ID, "feature"); err != nil || stored != "" {
		t.Fatalf("association survived retirement: %q (%v)", stored, err)
	}
	var afterRetire ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "feature", PreviousRunID: replaced.RunID}, &afterRetire); err != nil {
		t.Fatal(err)
	}
	ordinary := waitForRunTerminalState(t, d, afterRetire.RunID)
	if ordinary.ExistingPRURL != nil {
		t.Fatalf("retired branch still carries an association: %+v", ordinary.ExistingPRURL)
	}
}

func writeCommit(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "commit "+name)
}

// The gate branch is the registered repository's custody record. An explicit
// submission may advance it, but a head the submission does not contain belongs
// to work the launch must not discard.
func TestExistingPRLaunchRefusesToRewriteAnUnrelatedGateBranch(t *testing.T) {
	bin := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, name), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake gh: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	const target = "https://github.com/upstream/widgets/pull/168"
	var calls atomic.Int32
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&existingPRObserveStep{calls: &calls, target: target}} })
	repo, _ := setupTestGitRepo(t, p, d, "explicit-pr-custody")
	repo, err := d.UpdateRepoForkURL(repo.ID, "https://github.com/contributor/widgets.git")
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "new.txt"), []byte("new committed work"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "new work")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "unrelated.txt"), []byte("another workstream"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "unrelated work")
	unrelated := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "reset", "--hard", head)
	gateDir := p.RepoDir(repo.ID)
	gitCmd(t, gateDir, "fetch", "--no-tags", "--", repo.WorkingPath, unrelated)
	gitCmd(t, gateDir, "update-ref", "refs/heads/feature", unrelated)
	payload := fmt.Sprintf(`{"number":168,"html_url":%q,"state":"open","base":{"ref":"main","repo":{"full_name":"upstream/widgets","html_url":"https://github.com/upstream/widgets"}},"head":{"ref":"feature","sha":%q,"repo":{"full_name":"contributor/widgets","html_url":"https://github.com/contributor/widgets"}}}`, target, head)
	t.Setenv("FAKE_CLI_EXISTING_PR_JSON", payload)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Call(ipc.MethodStartExistingPRRun, &ipc.StartExistingPRRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "validate existing upstream PR", URL: target}, &ipc.RerunResult{}); err == nil {
		t.Fatal("explicit launch discarded an unrelated gate head")
	}
	if got := gitOutput(t, gateDir, "rev-parse", "refs/heads/feature^{commit}"); got != unrelated {
		t.Fatalf("gate branch = %s, want the preserved head %s", got, unrelated)
	}
	if calls.Load() != 0 {
		t.Fatalf("refused launch ran the pipeline: %d", calls.Load())
	}
}

type existingPRObserveStep struct {
	calls  *atomic.Int32
	target string
	base   string
}

func (s *existingPRObserveStep) Name() types.StepName { return types.StepReview }
func (s *existingPRObserveStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls.Add(1)
	if ctx.Run.ExistingPRURL == nil || *ctx.Run.ExistingPRURL != s.target || ctx.Run.PRURL == nil || *ctx.Run.PRURL != s.target {
		return nil, fmt.Errorf("target missing before first step")
	}
	if ctx.Run.BaseSHA == "" {
		return nil, fmt.Errorf("run has no base commit to diff from")
	}
	if s.base != "" && (ctx.Run.PRBaseBranch == nil || *ctx.Run.PRBaseBranch != s.base) {
		return nil, fmt.Errorf("step ran against %v, not the pull request base %s", ctx.Run.PRBaseBranch, s.base)
	}
	return &pipeline.StepOutcome{}, nil
}
