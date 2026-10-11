package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestReviewStep_UsesExistingPRTargetInsteadOfRepositoryDefault(t *testing.T) {
	dir, mainSHA, _ := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "main")
	gitCmd(t, dir, "checkout", "-b", "develop")
	if err := os.WriteFile(filepath.Join(dir, "develop.txt"), []byte("integration change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "develop.txt")
	gitCmd(t, dir, "commit", "-m", "integration change")
	developSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "rebase", "develop")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	findings := cleanReviewFindings()
	findings.ReviewedPaths = []string{"feature.txt", "develop.txt"}
	payload, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{name: "reviewer", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: payload}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, mainSHA, headSHA, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Env, _ = fakeGHWithBase(t, prURL, "develop")

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("review agent was not called")
	}
	want := "branch changes between " + developSHA + " and " + headSHA
	if !strings.Contains(ag.calls[0].Prompt, want) {
		t.Fatalf("review prompt did not use the live PR target; want %q", want)
	}
}
