package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

type targetContractFixture struct {
	dir       string
	upstream  string
	masterSHA string
	testSHA   string
	headSHA   string
}

func newTargetContractFixture(t *testing.T) targetContractFixture {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "master")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "master base")
	masterSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "master")

	gitCmd(t, dir, "checkout", "-b", "test")
	for i := 0; i < 4; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("target-%d.txt", i)), []byte("target history\n"), 0o644)
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", fmt.Sprintf("target history %d", i))
	}
	testSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "test")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("one intended commit\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	return targetContractFixture{dir: dir, upstream: upstream, masterSHA: masterSHA, testSHA: testSHA, headSHA: headSHA}
}

func (f targetContractFixture) context(t *testing.T, ag agent.Agent, commands config.Commands) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, f.dir, f.masterSHA, f.headSHA, commands)
	sctx.Repo.DefaultBranch = "master"
	sctx.Repo.UpstreamURL = f.upstream
	sctx.Run.TargetBranch = "test"
	sctx.Run.TargetSHA = f.testSHA
	return sctx
}

func TestBaseSensitiveAgentPhasesUseOnlyRunTarget(t *testing.T) {
	t.Parallel()
	fixture := newTargetContractFixture(t)
	tests := []struct {
		name     string
		commands config.Commands
		output   json.RawMessage
		run      func(*pipeline.StepContext) error
	}{
		{
			name:   "review",
			output: json.RawMessage(`{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"bounded","risk_scope":"source-or-external"}`),
			run: func(sctx *pipeline.StepContext) error {
				_, err := (&ReviewStep{}).Execute(sctx)
				return err
			},
		},
		{
			name:   "test",
			output: json.RawMessage(`{"findings":[],"summary":"clean","tested":["focused feature check"],"testing_summary":"focused check passed","artifacts":[]}`),
			run: func(sctx *pipeline.StepContext) error {
				_, err := (&TestStep{}).Execute(sctx)
				return err
			},
		},
		{
			name:     "document",
			commands: config.Commands{Lint: "true"},
			output:   json.RawMessage(`{"findings":[],"summary":"clean"}`),
			run: func(sctx *pipeline.StepContext) error {
				_, err := (&DocumentStep{}).Execute(sctx)
				return err
			},
		},
		{
			name:   "lint",
			output: json.RawMessage(`{"findings":[],"summary":"clean"}`),
			run: func(sctx *pipeline.StepContext) error {
				_, err := (&LintStep{}).Execute(sctx)
				return err
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var prompt string
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				prompt = opts.Prompt
				return &agent.Result{Output: tc.output}, nil
			}}
			sctx := fixture.context(t, ag, tc.commands)
			if err := tc.run(sctx); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(prompt, "base commit: "+fixture.testSHA) || !strings.Contains(prompt, "target branch: test") {
				t.Fatalf("%s prompt does not carry run target test@%s:\n%s", tc.name, fixture.testSHA, prompt)
			}
			if strings.Contains(prompt, "base commit: "+fixture.masterSHA) || strings.Contains(prompt, "target branch: master") {
				t.Fatalf("%s prompt fell back to repository default master@%s:\n%s", tc.name, fixture.masterSHA, prompt)
			}
			files := gitCmd(t, fixture.dir, "diff", "--name-only", fixture.testSHA+".."+fixture.headSHA)
			if files != "feature.txt" {
				t.Fatalf("%s target-scoped agent diff = %q, want feature.txt", tc.name, files)
			}
		})
	}
}

func TestRebaseStepUsesPinnedRunTargetInsteadOfRepositoryDefault(t *testing.T) {
	t.Parallel()
	fixture := newTargetContractFixture(t)
	sctx := fixture.context(t, &mockAgent{name: "test"}, config.Commands{})
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.SkipRemaining {
		t.Fatal("feature commit was incorrectly treated as empty against target")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "target branch test pinned at "+fixture.testSHA) || !strings.Contains(joined, "already ahead of "+fixture.testSHA) {
		t.Fatalf("rebase did not use pinned test target:\n%s", joined)
	}
	if strings.Contains(joined, "origin/master") || strings.Contains(joined, fixture.masterSHA) {
		t.Fatalf("rebase fell back to repository default:\n%s", joined)
	}
}

func TestIntentScopeUsesPinnedRunTargetInsteadOfRepositoryDefault(t *testing.T) {
	t.Parallel()
	fixture := newTargetContractFixture(t)
	sctx := fixture.context(t, &mockAgent{name: "test"}, config.Commands{})

	baseSHA, err := resolveRunIntentBaseSHA(context.Background(), sctx, fixture.dir)
	if err != nil {
		t.Fatal(err)
	}
	if baseSHA != fixture.testSHA {
		t.Fatalf("intent base = %s, want target test SHA %s (master %s)", baseSHA, fixture.testSHA, fixture.masterSHA)
	}
	files, err := diffFilesForIntentMatching(context.Background(), fixture.dir, baseSHA, fixture.headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(files, ","); got != "feature.txt" {
		t.Fatalf("intent diff inputs = %q, want only feature.txt", got)
	}
}
