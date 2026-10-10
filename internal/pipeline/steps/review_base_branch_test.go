package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// setupIntegrationTargetRepo builds the explicit-target shape: main (1 file)
// -> integration (+2 files) -> feature (+1 file), with HEAD on feature and a
// hermetic origin (the worktree itself, via ensureHermeticOrigin) so base
// resolution fetches stay local. Returns workDir, mainTip, integrationTip,
// headSHA.
func setupIntegrationTargetRepo(t *testing.T) (workDir, mainTip, integrationTip, headSHA string) {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	writeFixtureFile(t, dir, "base.txt", "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main base")
	mainTip = gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "integration")
	writeFixtureFile(t, dir, "integration01.txt", "integration\n")
	writeFixtureFile(t, dir, "integration02.txt", "integration\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "integration work")
	integrationTip = gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature")
	writeFixtureFile(t, dir, "feature.txt", "feature\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	return dir, mainTip, integrationTip, headSHA
}

func cleanFindingsCovering(t *testing.T, paths ...string) []byte {
	t.Helper()
	findings := cleanReviewFindings()
	findings.ReviewedPaths = paths
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	return findingsJSON
}

// TestReviewStep_ScopesAgainstRecordedExplicitTarget pins the review-base fix:
// when the run records an explicit integration target (runs.pr_base_branch),
// the review step must scope against that target - the same effective base the
// rebase, PR, and CI steps use - not the repository default branch. Scoping
// against main here would draft the integration branch's files into the
// review (3 reviewable files instead of 1) and park the head for approval on
// coverage the reviewer was never asked to produce.
func TestReviewStep_ScopesAgainstRecordedExplicitTarget(t *testing.T) {
	t.Parallel()
	dir, mainTip, integrationTip, headSHA := setupIntegrationTargetRepo(t)

	findingsJSON := cleanFindingsCovering(t, "feature.txt")
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	// The recorded push oldrev is the integration tip (non-zero): the step
	// must reach the target through the recorded explicit base branch, not
	// through Run.BaseSHA (a legacy fallback) and not through main.
	sctx := newTestContextWithDBRecords(t, ag, dir, integrationTip, headSHA, config.Commands{})
	target := "integration"
	sctx.Run.PRBaseBranch = &target

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("NeedsApproval = true with full coverage of the feature delta; reviewable = %v (want [feature.txt])", outcome.ReviewablePaths)
	}
	if !slices.Equal(outcome.ReviewablePaths, []string{"feature.txt"}) {
		t.Fatalf("reviewable = %v, want [feature.txt] (integration files leaked in from the wrong base)", outcome.ReviewablePaths)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the review analyzer to run")
	}
	wantScope := fmt.Sprintf("branch changes between %s and %s", integrationTip, headSHA)
	if !strings.Contains(ag.calls[0].Prompt, wantScope) {
		t.Fatalf("review prompt does not scope against the recorded target %q (main tip %s)", target, mainTip)
	}
	if !strings.Contains(ag.calls[0].Prompt, "- base branch: integration") {
		t.Fatalf("review prompt does not name the recorded target as its base branch")
	}
}

// TestReviewStep_ScopesAgainstDefaultBranchWithoutExplicitTarget guards the
// fallback: with no recorded explicit target, review scoping is unchanged and
// still covers the full branch delta against the repository default branch.
func TestReviewStep_ScopesAgainstDefaultBranchWithoutExplicitTarget(t *testing.T) {
	t.Parallel()
	dir, mainTip, _, headSHA := setupIntegrationTargetRepo(t)

	findingsJSON := cleanFindingsCovering(t, "integration01.txt", "integration02.txt", "feature.txt")
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, mainTip, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("NeedsApproval = true with full coverage of the branch delta; reviewable = %v", outcome.ReviewablePaths)
	}
	if !slices.Equal(slices.Sorted(slices.Values(outcome.ReviewablePaths)), []string{"feature.txt", "integration01.txt", "integration02.txt"}) {
		t.Fatalf("reviewable = %v, want the full branch delta vs main", outcome.ReviewablePaths)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the review analyzer to run")
	}
	wantScope := fmt.Sprintf("branch changes between %s and %s", mainTip, headSHA)
	if !strings.Contains(ag.calls[0].Prompt, wantScope) {
		t.Fatalf("review prompt does not scope against the default branch without an explicit target")
	}
}
