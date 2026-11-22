package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRequireActionRequiresCompletedRebase(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	const otherHead = "fedcba9876543210fedcba9876543210fedcba98"
	const body = `## Pipeline

Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)

<!-- no-mistakes-pipeline-attestation:v1 {"head_sha":"%s","steps":[%s{"step":"review","status":"completed"},{"step":"test","status":"completed"},{"step":"document","status":"completed"}]} -->
`
	tests := []struct {
		name         string
		rebaseStatus string
		prHead       string
		wantFailure  string
	}{
		{name: "omitted", prHead: head, wantFailure: "rebase (missing)"},
		{name: "incomplete", rebaseStatus: "running", prHead: head, wantFailure: "rebase (status=running)"},
		{name: "different head", rebaseStatus: "completed", prHead: otherHead, wantFailure: "does not match"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var rebase string
			if tc.rebaseStatus != "" {
				rebase = fmt.Sprintf(`{"step":"rebase","status":%q},`, tc.rebaseStatus)
			}
			got := runRequireAction(t, actionRun{body: fmt.Sprintf(body, head, rebase), headSHA: tc.prHead})
			assertRebaseActionVerdict(t, got, tc.wantFailure)
		})
	}
}

func TestRequireActionWithRecordedRebase(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent.gitconfig"))

	tests := []struct {
		name        string
		startRef    string
		wantRef     string
		skipRebase  bool
		wantFailure string
	}{
		{name: "equal head", startRef: "origin/topic", wantRef: "origin/topic"},
		{name: "already ahead", startRef: "ahead", wantRef: "ahead"},
		{name: "fast-forward", startRef: "origin/main", wantRef: "origin/topic"},
		{name: "skipped", startRef: "origin/topic", wantRef: "origin/topic", skipRebase: true, wantFailure: "rebase (status=skipped)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream, workDir := newRebaseActionHistory(t)
			runGit := func(args ...string) string {
				return rebaseActionGit(t, workDir, args...)
			}
			runGit("checkout", "--detach", tc.startRef)
			startHead := runGit("rev-parse", "HEAD")
			wantHead := runGit("rev-parse", tc.wantRef)

			p := paths.WithRoot(t.TempDir())
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { database.Close() })
			repo, err := database.InsertRepo(workDir, upstream, "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "topic", startHead, runGit("rev-parse", "origin/main"))
			if err != nil {
				t.Fatal(err)
			}
			executor := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{&steps.RebaseStep{}}, nil)
			if tc.skipRebase {
				executor.SetSkippedSteps([]types.StepName{types.StepRebase})
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			if err := executor.Execute(ctx, run, repo, workDir); err != nil {
				t.Fatal(err)
			}
			records, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, stepName := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument} {
				records = append(records, &db.StepResult{StepName: stepName, Status: types.StepStatusCompleted})
			}
			if head := runGit("rev-parse", "HEAD"); head != wantHead {
				t.Fatalf("worktree head after Rebase = %s, want %s", head, wantHead)
			}
			body, _ := steps.BuildPipelineSummary(records, nil, run.HeadSHA)
			got := runRequireAction(t, actionRun{body: body, headSHA: wantHead})
			assertRebaseActionVerdict(t, got, tc.wantFailure)
		})
	}
}

func newRebaseActionHistory(t *testing.T) (upstream, workDir string) {
	t.Helper()
	upstream = t.TempDir()
	rebaseActionGit(t, upstream, "init", "--initial-branch=main", "--template=")
	rebaseActionGit(t, upstream, "commit", "--allow-empty", "-m", "base")
	rebaseActionGit(t, upstream, "checkout", "-b", "topic")
	if err := os.WriteFile(filepath.Join(upstream, "topic.txt"), []byte(t.Name()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rebaseActionGit(t, upstream, "add", "topic.txt")
	rebaseActionGit(t, upstream, "commit", "-m", "published topic change")

	workDir = t.TempDir()
	rebaseActionGit(t, workDir, "clone", "--template=", "--branch", "topic", "--", upstream, ".")
	rebaseActionGit(t, workDir, "checkout", "-b", "ahead")
	rebaseActionGit(t, workDir, "commit", "--allow-empty", "-m", "subsequent topic commit")
	return upstream, workDir
}

func rebaseActionGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	args = append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com"}, args...)
	out, err := git.Run(t.Context(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertRebaseActionVerdict(t *testing.T, got actionResult, wantFailure string) {
	t.Helper()
	if wantFailure == "" {
		if got.conclusion != "success" || got.outputs["compliant"] != "true" {
			t.Fatalf("expected compliant success, got %q and compliant=%q\n%s", got.conclusion, got.outputs["compliant"], got.output)
		}
		return
	}
	if got.conclusion != "failure" || got.outputs["compliant"] != "false" || !strings.Contains(got.output, wantFailure) {
		t.Fatalf("expected a noncompliant failure naming %q, got %q and compliant=%q\n%s", wantFailure, got.conclusion, got.outputs["compliant"], got.output)
	}
}
