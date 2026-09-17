//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// existingPRWorld is the reproduction shape `--existing-pr` exists for: the
// checked-out clone's origin is the contributor's FORK, while the pull request
// being validated lives in the upstream repository. Both github.com URLs are
// rewritten to local bare repositories, so every fetch and push the pipeline
// makes is real git against the repository it claims to be talking to.
type existingPRWorld struct {
	h            *Harness
	forkDir      string // the fork bare repo: origin, and where the pipeline pushes
	upstreamDir  string // the upstream bare repo the pull request lives in
	branch       string
	prURL        string
	ghLog        string
	prBodyFile   string
	prTitleFile  string
	configPath   string
	upstreamRepo string
	forkRepo     string
}

const (
	existingPRUpstreamRepo = "upstream-owner/widgets"
	existingPRForkRepo     = "fork-owner/widgets"
	existingPRNumber       = "168"
	existingPRBranch       = "fm/account-context"
)

func newExistingPRWorld(t *testing.T) *existingPRWorld {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	ctx := context.Background()

	// The harness upstream becomes the UPSTREAM repository; a new bare repo
	// becomes the fork that origin points at.
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}

	upstreamURL := "https://github.com/" + existingPRUpstreamRepo + ".git"
	forkURL := "https://github.com/" + existingPRForkRepo + ".git"
	configureGitURLRewrite(t, h, upstreamURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", forkURL); err != nil {
		t.Fatalf("point origin at the fork: %v\n%s", err, out)
	}

	stateDir := t.TempDir()
	w := &existingPRWorld{
		h:            h,
		forkDir:      forkDir,
		upstreamDir:  h.UpstreamDir,
		branch:       existingPRBranch,
		prURL:        "https://github.com/" + existingPRUpstreamRepo + "/pull/" + existingPRNumber,
		ghLog:        filepath.Join(stateDir, "gh.log"),
		prBodyFile:   filepath.Join(stateDir, "pr-body.md"),
		prTitleFile:  filepath.Join(stateDir, "pr-title.txt"),
		upstreamRepo: existingPRUpstreamRepo,
		forkRepo:     existingPRForkRepo,
	}
	if err := os.WriteFile(w.prBodyFile, []byte("Adds account context to the quota reader.\n"), 0o644); err != nil {
		t.Fatalf("seed PR body: %v", err)
	}
	if err := os.WriteFile(w.prTitleFile, []byte("Adds account context to the quota reader"), 0o644); err != nil {
		t.Fatalf("seed PR title: %v", err)
	}

	// The daemon is long-lived and inherits this process's environment once,
	// so what the forge answers lives in a file the stub re-reads on every
	// invocation and this test can change mid-journey.
	w.configPath = filepath.Join(stateDir, "gh-pr.json")
	w.writeForgeState(t, func(*existingPRForgeState) {})
	t.Setenv("FAKEAGENT_GH_MODE", "existing-pr")
	t.Setenv("FAKEAGENT_GH_LOG", w.ghLog)
	t.Setenv("FAKEAGENT_GH_PR_CONFIG", w.configPath)

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	return w
}

// existingPRForgeState mirrors the gh stub's configuration file: what the
// upstream pull request currently looks like to the product.
type existingPRForgeState struct {
	PRRepo     string `json:"pr_repo"`
	PRNumber   string `json:"pr_number"`
	BaseRef    string `json:"base_ref"`
	SourceRepo string `json:"source_repo"`
	SourceRef  string `json:"source_ref"`
	SourceDir  string `json:"source_dir"`
	HeadSHA    string `json:"head_sha"`
	State      string `json:"state"`
	ViewState  string `json:"view_state"`
	Merged     bool   `json:"merged"`
	BodyFile   string `json:"body_file"`
	TitleFile  string `json:"title_file"`
	CreatedPR  string `json:"created_pr"`
}

// writeForgeState publishes the pull request's current shape, applying mutate
// on top of the healthy default so each case states only what it changes.
func (w *existingPRWorld) writeForgeState(t *testing.T, mutate func(*existingPRForgeState)) {
	t.Helper()
	state := existingPRForgeState{
		PRRepo:     existingPRUpstreamRepo,
		PRNumber:   existingPRNumber,
		BaseRef:    "main",
		SourceRepo: existingPRForkRepo,
		SourceRef:  w.branch,
		SourceDir:  w.forkDir,
		State:      "open",
		BodyFile:   w.prBodyFile,
		TitleFile:  w.prTitleFile,
		CreatedPR:  "https://github.com/" + existingPRForkRepo + "/pull/1",
	}
	mutate(&state)
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode forge state: %v", err)
	}
	if err := os.WriteFile(w.configPath, encoded, 0o644); err != nil {
		t.Fatalf("write forge state: %v", err)
	}
}

// commitAndPublish makes a commit on the source branch and publishes it to the
// fork, which is what `--existing-pr` requires an operator to have done: the
// head it names must already be the pull request's live source head.
func (w *existingPRWorld) commitAndPublish(t *testing.T, path, content, message string) string {
	t.Helper()
	sha := w.h.CommitChange(w.branch, path, content, message)
	if out, err := w.h.runGit(context.Background(), w.h.WorkDir, "push", w.forkDir, w.branch); err != nil {
		t.Fatalf("publish %s to fork: %v\n%s", w.branch, err, out)
	}
	return sha
}

// drive runs the binary in the working clone with room for a whole pipeline.
// Harness.Run caps at a minute, which a full nine-step run exceeds here.
func (w *existingPRWorld) drive(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.h.NMBin, args...)
	cmd.Dir = w.h.WorkDir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	w.h.syncDaemonOwnership()
	return string(out), err
}

func (w *existingPRWorld) ghInvocations(t *testing.T) []ghStubInvocation {
	t.Helper()
	if _, err := os.Stat(w.ghLog); err != nil {
		return nil
	}
	return readGHStubInvocations(t, w.ghLog)
}

// settledRun resolves the run a drive call just returned from and asserts it
// reached the point AXI hands control back at. The pull request stays OPEN -
// an association to a merged one is refused, which is the whole point - so
// that point is CI readiness: `axi run` returns `checks-passed` while the CI
// step keeps monitoring for the human merge, and the run is still running
// rather than completed.
func (w *existingPRWorld) settledRun(t *testing.T, stage, out string) *ipc.RunInfo {
	t.Helper()
	if !strings.Contains(out, "outcome: checks-passed") {
		t.Fatalf("%s: drive did not return at CI readiness:\n%s", stage, out)
	}
	run := w.h.RunInfo(latestRunID(t, w.h))
	if run.Status != types.RunRunning {
		t.Fatalf("%s: run status = %s (error %v), want %s while CI monitoring continues\n%s", stage, run.Status, deref(run.Error), types.RunRunning, out)
	}
	return run
}

// refuseAnyPRCreate is the invariant the whole feature turns on: an associated
// run must never open the accidental fork pull request the reproduction saw.
func (w *existingPRWorld) refuseAnyPRCreate(t *testing.T, stage string) {
	t.Helper()
	for _, inv := range w.ghInvocations(t) {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
			t.Fatalf("%s: created an alternate pull request instead of using the explicit target: %+v", stage, inv)
		}
	}
}

func (w *existingPRWorld) prBody(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(w.prBodyFile)
	if err != nil {
		t.Fatalf("read PR body: %v", err)
	}
	return string(data)
}

func (w *existingPRWorld) prTitle(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(w.prTitleFile)
	if err != nil {
		t.Fatalf("read PR title: %v", err)
	}
	return string(data)
}

// TestExistingUpstreamPRAssociationJourney drives the full documented
// lifecycle against a real daemon: establish the association, reuse it from an
// unflagged launch, see the associated-branch skip refusal, retire it, and see
// ordinary discovery return afterwards.
func TestExistingUpstreamPRAssociationJourney(t *testing.T) {
	w := newExistingPRWorld(t)
	h := w.h
	ctx := context.Background()

	head := w.commitAndPublish(t, "quota.go", "package quota // account context\n", "add account context")

	out, err := w.drive(t, "axi", "run", "--existing-pr", w.prURL, "--intent", "validate the account-context change in the existing upstream PR", "--yes")
	if err != nil {
		t.Fatalf("establish association: %v\n%s", err, out)
	}
	run := w.settledRun(t, "establish", out)
	if run.PRURL == nil || *run.PRURL != w.prURL {
		t.Fatalf("run PR URL = %v, want the explicit upstream target %s\n%s", run.PRURL, w.prURL, out)
	}
	w.refuseAnyPRCreate(t, "establish")

	// The pipeline published to the FORK, never to the upstream repository.
	if _, err := h.runGit(ctx, w.upstreamDir, "rev-parse", "--verify", "refs/heads/"+w.branch); err == nil {
		t.Fatalf("upstream repository unexpectedly received the source branch")
	}
	forkTip, err := h.runGit(ctx, w.forkDir, "rev-parse", "refs/heads/"+w.branch)
	if err != nil {
		t.Fatalf("fork branch missing: %v\n%s", err, forkTip)
	}
	if got := strings.TrimSpace(string(forkTip)); got != run.HeadSHA {
		t.Fatalf("fork branch at %s, want the run head %s", got, run.HeadSHA)
	}
	if head == "" {
		t.Fatalf("no submitted head recorded")
	}

	// Every write reached the upstream pull request, by number.
	var sawUpstreamEdit bool
	for _, inv := range w.ghInvocations(t) {
		if len(inv.Args) >= 3 && inv.Args[0] == "pr" && inv.Args[1] == "edit" && inv.Args[2] == existingPRNumber && inv.Repo == existingPRUpstreamRepo {
			sawUpstreamEdit = true
		}
	}
	if !sawUpstreamEdit {
		t.Fatalf("no `gh pr edit %s --repo %s` in the stub log: %+v", existingPRNumber, existingPRUpstreamRepo, w.ghInvocations(t))
	}

	// The author's title and narrative survive; the run only appends.
	if got := w.prTitle(t); got != "Adds account context to the quota reader" {
		t.Fatalf("PR title was redrafted: %q", got)
	}
	body := w.prBody(t)
	if !strings.Contains(body, "Adds account context to the quota reader.") {
		t.Fatalf("author narrative lost from PR body:\n%s", body)
	}
	if !strings.Contains(body, "no-mistakes") {
		t.Fatalf("no no-mistakes appendix appended to the PR body:\n%s", body)
	}

	// Reuse: a later launch with NO flag publishes to the same pull request.
	w.commitAndPublish(t, "quota.go", "package quota // account context, revised\n", "revise account context")
	reuseOut, err := w.drive(t, "axi", "run", "--intent", "revise the account-context change", "--yes")
	if err != nil {
		t.Fatalf("reuse association: %v\n%s", err, reuseOut)
	}
	reuse := w.settledRun(t, "reuse", reuseOut)
	if reuse.PRURL == nil || *reuse.PRURL != w.prURL {
		t.Fatalf("unflagged reuse run PR URL = %v, want %s\n%s", reuse.PRURL, w.prURL, reuseOut)
	}
	w.refuseAnyPRCreate(t, "reuse")

	// An associated branch refuses a partial run rather than updating someone
	// else's pull request from an unvalidated head.
	w.commitAndPublish(t, "quota.go", "package quota // account context, third\n", "third account context revision")
	skipOut, skipErr := w.drive(t, "axi", "run", "--intent", "skip the lint gate", "--skip", "lint")
	if skipErr == nil {
		t.Fatalf("--skip accepted on an associated branch:\n%s", skipOut)
	}
	if !strings.Contains(skipOut, "--retire-existing-pr") {
		t.Fatalf("skip refusal does not name the retire command:\n%s", skipOut)
	}

	// Retire, then ordinary repository-scoped discovery returns. Retiring is
	// refused while a run is active, so the CI-monitoring run this journey
	// left behind is ended first - an in-flight run must never have where it
	// publishes changed under it.
	if abortOut, abortErr := h.Run("axi", "abort"); abortErr != nil {
		t.Fatalf("abort the monitoring run before retiring: %v\n%s", abortErr, abortOut)
	}
	retireOut, err := w.drive(t, "axi", "run", "--retire-existing-pr")
	if err != nil {
		t.Fatalf("retire association: %v\n%s", err, retireOut)
	}
	if !strings.Contains(retireOut, w.prURL) {
		t.Fatalf("retire output does not report the removed association:\n%s", retireOut)
	}
	afterOut, err := w.drive(t, "axi", "run", "--intent", "validate after retiring the association", "--yes")
	if err != nil {
		t.Fatalf("run after retirement: %v\n%s", err, afterOut)
	}
	after := w.settledRun(t, "after retirement", afterOut)
	if after.PRURL != nil && *after.PRURL == w.prURL {
		t.Fatalf("retired branch still published to %s\n%s", w.prURL, afterOut)
	}
	var sawCreateAfterRetire bool
	for _, inv := range w.ghInvocations(t) {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
			sawCreateAfterRetire = true
		}
	}
	if !sawCreateAfterRetire {
		t.Fatalf("retired branch did not return to ordinary discovery:\n%s", afterOut)
	}
}

// TestExistingUpstreamPRRefusesMismatchedTarget is the adversarial half: a
// target whose live head or whose source repository does not match must fail
// the launch and must not reach for another pull request.
func TestExistingUpstreamPRRefusesMismatchedTarget(t *testing.T) {
	w := newExistingPRWorld(t)

	w.commitAndPublish(t, "quota.go", "package quota // account context\n", "add account context")

	// The pull request's source branch moved outside this run.
	w.writeForgeState(t, func(s *existingPRForgeState) {
		s.HeadSHA = "0123456789abcdef0123456789abcdef01234567"
	})
	out, err := w.drive(t, "axi", "run", "--existing-pr", w.prURL, "--intent", "validate against a head the PR does not have", "--yes")
	if err == nil {
		t.Fatalf("stale head accepted:\n%s", out)
	}
	w.refuseAnyPRCreate(t, "head mismatch")

	// The pull request's source is someone else's repository.
	w.writeForgeState(t, func(s *existingPRForgeState) { s.SourceRepo = "someone-else/widgets" })
	foreignOut, err := w.drive(t, "axi", "run", "--existing-pr", w.prURL, "--intent", "validate a PR sourced from another repository", "--yes")
	if err == nil {
		t.Fatalf("foreign source repository accepted:\n%s", foreignOut)
	}
	w.refuseAnyPRCreate(t, "foreign source repository")

	// A closed pull request is not a usable review object.
	w.writeForgeState(t, func(s *existingPRForgeState) { s.State = "closed" })
	closedOut, err := w.drive(t, "axi", "run", "--existing-pr", w.prURL, "--intent", "validate against a closed pull request", "--yes")
	if err == nil {
		t.Fatalf("closed pull request accepted:\n%s", closedOut)
	}
	w.refuseAnyPRCreate(t, "closed pull request")
}

// latestRunID returns the most recently created run for the harness repo.
func latestRunID(t *testing.T, h *Harness) string {
	t.Helper()
	runs := h.Runs()
	if len(runs) == 0 {
		t.Fatalf("no runs recorded")
	}
	newest := runs[0]
	for _, run := range runs[1:] {
		if run.CreatedAt > newest.CreatedAt {
			newest = run
		}
	}
	return newest.ID
}

// TestExistingUpstreamPRIntegratesABaseSharingTheSourceBranchName is the
// name-collision shape: the pull request targets an upstream branch whose NAME
// equals the fork source branch's. Two bare branch names are not the same ref
// once the integration base lives in another repository, so the run must still
// integrate with the upstream branch instead of reading itself as already
// integrated and validating a head that was never measured against the base.
func TestExistingUpstreamPRIntegratesABaseSharingTheSourceBranchName(t *testing.T) {
	w := newExistingPRWorld(t)
	h := w.h
	ctx := context.Background()

	// The UPSTREAM repository gets a branch named exactly like the fork's
	// source branch, carrying a commit the source branch does not have.
	upstreamOnly := h.CommitChange("upstream-base-seed", "upstream-note.txt", "the upstream base moved\n", "advance the upstream base")
	if out, err := h.runGit(ctx, h.WorkDir, "push", w.upstreamDir, "upstream-base-seed:refs/heads/"+w.branch); err != nil {
		t.Fatalf("seed the upstream base branch: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "main"); err != nil {
		t.Fatalf("return to main: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "branch", "-D", "upstream-base-seed"); err != nil {
		t.Fatalf("drop the seed branch: %v\n%s", err, out)
	}
	w.writeForgeState(t, func(s *existingPRForgeState) { s.BaseRef = w.branch })

	w.commitAndPublish(t, "quota.go", "package quota // account context\n", "add account context")

	out, err := w.drive(t, "axi", "run", "--existing-pr", w.prURL, "--intent", "validate against an upstream base that shares the source branch name", "--yes")
	if err != nil {
		t.Fatalf("associated run with a same-named base: %v\n%s", err, out)
	}
	run := w.settledRun(t, "same-named base", out)
	w.refuseAnyPRCreate(t, "same-named base")

	// The validated head carries the upstream base commit, which is only true
	// if the run integrated with the upstream branch.
	forkTip, err := h.runGit(ctx, w.forkDir, "rev-parse", "refs/heads/"+w.branch)
	if err != nil {
		t.Fatalf("fork branch missing: %v\n%s", err, forkTip)
	}
	if got := strings.TrimSpace(string(forkTip)); got != run.HeadSHA {
		t.Fatalf("fork branch at %s, want the run head %s", got, run.HeadSHA)
	}
	if ancestryOut, err := h.runGit(ctx, w.forkDir, "merge-base", "--is-ancestor", upstreamOnly, run.HeadSHA); err != nil {
		t.Fatalf("validated head %s does not contain the upstream base commit %s - the run skipped integration: %v\n%s\n%s",
			run.HeadSHA, upstreamOnly, err, ancestryOut, out)
	}
}
