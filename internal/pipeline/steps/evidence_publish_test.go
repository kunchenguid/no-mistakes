package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const evidenceTestUpstream = "https://github.com/example/widgets.git"

// newEvidencePublishContext wires a run whose push target is a real local bare
// repository while the pipeline still sees a github.com URL, so the published
// branch and the links the PR body builds from it can both be asserted. The
// rewrite is git's own url.<base>.insteadOf, so every git call in the step goes
// through the same code path production uses.
func newEvidencePublishContext(t *testing.T, branch string) (sctx *pipeline.StepContext, remote string) {
	t.Helper()
	remote = t.TempDir()
	gitCmd(t, remote, "init", "--bare", "--initial-branch=main")

	dir := t.TempDir()
	gitCmd(t, dir, "init", "--initial-branch=main")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "config", "url."+remote+".insteadOf", evidenceTestUpstream)
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	gitCmd(t, dir, "remote", "add", "origin", evidenceTestUpstream)
	gitCmd(t, dir, "push", "origin", "main")

	baseSHA := gitCmd(t, dir, "rev-parse", "main")
	gitCmd(t, dir, "checkout", "-b", branch)
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx = newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = evidenceTestUpstream
	sctx.Repo.DefaultBranch = "main"
	sctx.Run.Branch = branch
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: ".no-mistakes/evidence", Branch: "no-mistakes/evidence"}
	return sctx, remote
}

func writeRunEvidence(t *testing.T, sctx *pipeline.StepContext, files map[string]string) {
	t.Helper()
	dir := testEvidenceDir(sctx)
	t.Cleanup(func() { os.RemoveAll(dir) })
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func testStepWithArtifacts(artifacts string) ([]*db.StepResult, map[string][]*db.StepRound) {
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[%s]}`, artifacts)
	steps := []*db.StepResult{
		{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings},
	}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}},
	}
	return steps, rounds
}

func TestPublishRunEvidence_LandsOnOrphanBranchAndLinksFromThePRBody(t *testing.T) {
	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	writeRunEvidence(t, sctx, map[string]string{
		"checkout.png": "\x89PNG binary",
		"cli-run.txt":  "it works\n",
	})
	evidenceDir := testEvidenceDir(sctx)
	branchHeadBefore := gitCmd(t, remote, "rev-parse", "refs/heads/main")

	links := publishRunEvidence(sctx)
	if links == nil {
		t.Fatal("expected evidence to be published")
	}

	tip := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence")
	tree := gitCmd(t, remote, "ls-tree", "-r", "--name-only", tip)
	for _, want := range []string{
		".no-mistakes/evidence/feature/add-login/checkout.png",
		".no-mistakes/evidence/feature/add-login/cli-run.txt",
	} {
		if !strings.Contains(tree, want) {
			t.Errorf("evidence branch missing %q, has:\n%s", want, tree)
		}
	}

	// The PR body links the published artifacts by evidence commit.
	steps, rounds := testStepWithArtifacts(fmt.Sprintf(
		`{"kind":"screenshot","label":"Checkout screenshot","path":%q},{"kind":"log","label":"CLI run","path":%q}`,
		filepath.Join(evidenceDir, "checkout.png"),
		filepath.Join(evidenceDir, "cli-run.txt"),
	))
	md := BuildTestingSummaryForPR(steps, rounds, sctx.Repo.UpstreamURL, sctx.Run.HeadSHA, sctx.WorkDir, testEvidenceDir(sctx), links)
	t.Logf("rendered PR testing markdown:\n%s", md)

	wantLink := "https://github.com/example/widgets/blob/" + tip + "/.no-mistakes/evidence/feature/add-login/checkout.png"
	if !strings.Contains(md, "- Evidence: [Checkout screenshot]("+wantLink+")") {
		t.Fatalf("expected the PR body to link the evidence branch, got:\n%s", md)
	}
	if !strings.Contains(md, "https://github.com/example/widgets/blob/"+tip+"/.no-mistakes/evidence/feature/add-login/cli-run.txt") {
		t.Fatalf("expected the log artifact to link the evidence branch, got:\n%s", md)
	}
	if strings.Contains(md, "local file:") {
		t.Fatalf("published evidence must not render as a local path, got:\n%s", md)
	}

	// Neither the default branch nor the run's own branch carries evidence.
	if after := gitCmd(t, remote, "rev-parse", "refs/heads/main"); after != branchHeadBefore {
		t.Errorf("main moved from %s to %s", branchHeadBefore, after)
	}
	if worktree := gitCmd(t, sctx.WorkDir, "status", "--porcelain"); worktree != "" {
		t.Errorf("publishing dirtied the worktree:\n%s", worktree)
	}
	if tracked := gitCmd(t, sctx.WorkDir, "ls-files"); strings.Contains(tracked, "evidence") {
		t.Errorf("the run branch tracks evidence files:\n%s", tracked)
	}
}

func TestPublishRunEvidence_PercentEncodesEveryArtifactPathSegment(t *testing.T) {
	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	sctx.Config.Test.Evidence.Dir = "evidence archive #1 100%"
	artifact := "capture #1 100%/checkout 100%.png"
	writeRunEvidence(t, sctx, map[string]string{artifact: "\x89PNG binary"})
	evidenceDir := testEvidenceDir(sctx)

	links := publishRunEvidence(sctx)
	if links == nil {
		t.Fatal("expected evidence to be published")
	}
	tip := gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence")
	steps, rounds := testStepWithArtifacts(fmt.Sprintf(
		`{"kind":"screenshot","label":"Encoded screenshot","path":%q}`,
		filepath.Join(evidenceDir, filepath.FromSlash(artifact)),
	))
	md := BuildTestingSummaryForPR(steps, rounds, sctx.Repo.UpstreamURL, sctx.Run.HeadSHA, sctx.WorkDir, testEvidenceDir(sctx), links)
	want := "https://github.com/example/widgets/blob/" + tip + "/evidence%20archive%20%231%20100%25/feature/add-login/capture%20%231%20100%25/checkout%20100%25.png"
	if !strings.Contains(md, want) {
		t.Fatalf("expected every artifact path segment to be encoded as %q, got:\n%s", want, md)
	}
}

func TestPublishRunEvidence_UsesTheConfiguredBranchName(t *testing.T) {
	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	sctx.Config.Test.Evidence.Branch = "team/ci/evidence"
	writeRunEvidence(t, sctx, map[string]string{"cli-run.txt": "it works\n"})

	if links := publishRunEvidence(sctx); links == nil {
		t.Fatal("expected evidence to be published")
	}

	tree := gitCmd(t, remote, "ls-tree", "-r", "--name-only", "refs/heads/team/ci/evidence")
	if !strings.Contains(tree, ".no-mistakes/evidence/feature/add-login/cli-run.txt") {
		t.Errorf("configured evidence branch missing the artifact, has:\n%s", tree)
	}
	if refs := gitCmd(t, remote, "for-each-ref", "--format=%(refname)", "refs/heads/no-mistakes"); refs != "" {
		t.Errorf("the default evidence branch was created too: %q", refs)
	}
}

func TestPublishRunEvidence_InvalidBranchNameFallsBackToLocalPaths(t *testing.T) {
	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	sctx.Config.Test.Evidence.Branch = "not a branch"
	writeRunEvidence(t, sctx, map[string]string{"cli-run.txt": "it works\n"})
	evidenceDir := testEvidenceDir(sctx)

	if links := publishRunEvidence(sctx); links != nil {
		t.Fatalf("expected no publication for an invalid branch name, got %+v", links)
	}
	if refs := gitCmd(t, remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Errorf("remote refs changed: %q", refs)
	}

	steps, rounds := testStepWithArtifacts(fmt.Sprintf(
		`{"kind":"screenshot","label":"Checkout screenshot","path":%q}`,
		filepath.Join(evidenceDir, "checkout.png"),
	))
	md := BuildTestingSummaryForPR(steps, rounds, sctx.Repo.UpstreamURL, sctx.Run.HeadSHA, sctx.WorkDir, testEvidenceDir(sctx), nil)
	if !strings.Contains(md, "local file:") {
		t.Fatalf("expected unpublished evidence to render as a local path, got:\n%s", md)
	}
}

func TestPublishRunEvidence_ProviderWithoutFileLinksPublishesNothing(t *testing.T) {
	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	sctx.Repo.UpstreamURL = "https://gitlab.com/example/widgets.git"
	writeRunEvidence(t, sctx, map[string]string{"cli-run.txt": "it works\n"})

	if links := publishRunEvidence(sctx); links != nil {
		t.Fatalf("expected no publication when no artifact link can be derived, got %+v", links)
	}
	if refs := gitCmd(t, remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Errorf("a branch was pushed that nothing could link to: %q", refs)
	}
}

func TestPublishRunEvidence_DisabledDoesNotTouchTheRemote(t *testing.T) {
	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	sctx.Config.Test.Evidence.StoreInRepo = false
	writeRunEvidence(t, sctx, map[string]string{"cli-run.txt": "it works\n"})

	if links := publishRunEvidence(sctx); links != nil {
		t.Fatalf("expected no publication when evidence storage is off, got %+v", links)
	}
	if refs := gitCmd(t, remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Errorf("remote refs changed: %q", refs)
	}
}
