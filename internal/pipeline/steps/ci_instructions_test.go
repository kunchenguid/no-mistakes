package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// captureCIFixPrompt drives one CI-fix round against a red check and returns
// the prompt the fix agent received.
func captureCIFixPrompt(t *testing.T, instructions string) string {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	env := fakeCIGH(t, "OPEN", `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`)

	var captured string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			captured = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	sctx.Config.CI.Instructions = instructions

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	sctx.Log = func(string) {}

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	driveCI(t, step, sctx)
	if captured == "" {
		t.Fatal("expected the CI-fix agent to be called with a prompt")
	}
	return captured
}

func TestCIStep_FixPromptCarriesCIInstructions(t *testing.T) {
	t.Parallel()

	prompt := captureCIFixPrompt(t, "  The windows job is slow; do not rerun it, read its log first.  ")
	if !strings.Contains(prompt, "Repository CI-fix instructions (trusted, from the default branch):") {
		t.Fatalf("prompt is missing the ci.instructions section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "The windows job is slow; do not rerun it, read its log first.") {
		t.Fatalf("prompt is missing the ci.instructions text:\n%s", prompt)
	}
	// The built-in repair rules stay in front of the repository's additions.
	rules := strings.Index(prompt, "you MUST produce file changes that fix it")
	section := strings.Index(prompt, "Repository CI-fix instructions")
	if rules < 0 || section < rules {
		t.Fatalf("ci.instructions must follow the built-in rules (rules at %d, section at %d)", rules, section)
	}
}

func TestCIStep_FixPromptWithoutCIInstructionsHasNoSection(t *testing.T) {
	t.Parallel()

	prompt := captureCIFixPrompt(t, "   ")
	if strings.Contains(prompt, "Repository CI-fix instructions") {
		t.Fatalf("empty ci.instructions must leave the prompt unchanged:\n%s", prompt)
	}
}
