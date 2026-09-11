package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type dlock31Publication struct {
	paths                                              *paths.Paths
	sctx                                               *pipeline.StepContext
	published, retained, remoteFile, bodyFile, logFile string
	baseEnv                                            []string
	agentCalls                                         int
}

func newDLOCK31Publication(t *testing.T) *dlock31Publication {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	source, base, published := setupGitRepo(t)
	f := &dlock31Publication{published: published, remoteFile: filepath.Join(t.TempDir(), "remote"), bodyFile: filepath.Join(t.TempDir(), "body"), logFile: filepath.Join(t.TempDir(), "commands")}
	ag := &mockAgent{name: "tripwire", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		f.agentCalls++
		return nil, errors.New("forbidden fixer invocation")
	}}
	f.sctx = newTestContextWithDBRecords(t, ag, source, base, published, config.Commands{})
	f.paths = paths.WithRoot(t.TempDir())
	if err := f.paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	gate, work := f.paths.RepoDir(f.sctx.Repo.ID), filepath.Join(t.TempDir(), "work")
	gitCmd(t, source, "clone", "--bare", "--no-hardlinks", source, gate)
	gitCmd(t, gate, "remote", "set-url", "origin", gate)
	gitCmd(t, gate, "worktree", "add", "--detach", work, published)
	f.sctx.WorkDir = work
	f.sctx.GateDir = gate
	if err := f.sctx.DB.UpdateRunStatus(f.sctx.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	f.sctx.Run.Status = types.RunRunning
	f.sctx.Config.CI.RevalidateRepairs = false
	recordReviewApproval(t, f.sctx, published)
	if err := f.sctx.DB.UpdateRunPushBinding(f.sctx.Run.ID, db.PushBinding{HeadSHA: published, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(gate), Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "repair.txt"), []byte("fixed"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "add", "repair.txt")
	gitCmd(t, work, "commit", "-m", "retained CI repair")
	f.retained = gitCmd(t, work, "rev-parse", "HEAD")
	prepareDLOCK31Gate(t, f)
	return f
}

func prepareDLOCK31Gate(t *testing.T, f *dlock31Publication) {
	t.Helper()
	sr, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	f.sctx.StepResultID, f.sctx.Fixing = sr.ID, true
	f.sctx.PreviousFindings = `{"findings":[{"id":"quota","severity":"error","action":"ask-user","description":"retained correction"}]}`
	for _, err := range []error{f.sctx.DB.StartStep(sr.ID), f.sctx.DB.SetStepFindings(sr.ID, f.sctx.PreviousFindings), f.sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusFixReview)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	prURL := "https://github.com/test/repo/pull/23"
	f.sctx.Run.PRURL = &prURL
	for path, content := range map[string]string{f.remoteFile: f.published, f.bodyFile: compliantPipelineBody(t, f.published)} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := fakeCLIBinDir(t)
	linkTestBinary(t, bin, "gh")
	linkTestBinary(t, bin, "git")
	f.baseEnv = fakeCLIEnv(bin, map[string]string{
		"FAKE_CLI_MODE": "ci-gh-local-publication", "FAKE_CLI_STATE": "OPEN", "FAKE_CLI_CHECKS": `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`,
		"FAKE_CLI_PR_HEAD_SHA": f.retained, "FAKE_CLI_PR_LIST_JSON": `[{"number":23,"url":"https://github.com/test/repo/pull/23","baseRefName":"main"}]`,
		"FAKE_CLI_PR_BODY_FILE": f.bodyFile, "FAKE_CLI_PR_TITLE": "CI repair", "FAKE_CLI_LOG": f.logFile, "FAKE_CLI_REMOTE_HEAD_FILE": f.remoteFile,
	})
	f.sctx.Env = append(append([]string{}, f.baseEnv...), "FAKE_CLI_PR_EDIT_ERR=fixture unavailable")
	_, err = (&CIStep{}).publishRepair(f.sctx, f.retained)
	if !errors.Is(err, errCIAttestationUnsettled) {
		t.Fatalf("initial attestation failure=%v", err)
	}
	binding, err := f.sctx.DB.RetainedCIRepair(f.sctx.Run.ID)
	if err != nil || binding == nil || binding.RetainedHead != f.retained {
		t.Fatalf("repair not bound: %+v %v", binding, err)
	}
	f.sctx.Env = f.baseEnv
}

// The real publication path runs, but its Git transport is a temporary file.
func TestDLOCK31RetainedAttestationRepairDoesNotInvokeFixer(t *testing.T) {
	f := newDLOCK31Publication(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.sctx.Ctx = ctx
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { cancel(); return ctx.Err() }}
	_, _ = step.Execute(f.sctx)
	stored, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.HeadSHA != f.retained || stored.LastPushedSHA == nil || *stored.LastPushedSHA != f.retained || stored.PushGeneration == nil || *stored.PushGeneration != 2 {
		t.Fatalf("publication not durably settled: %+v", stored)
	}
	if f.agentCalls != 0 {
		t.Fatalf("fixer calls=%d", f.agentCalls)
	}
	if gitCmd(t, f.sctx.WorkDir, "rev-parse", "HEAD") != f.retained || gitStatusPorcelain(t, f.sctx.WorkDir) != "" {
		t.Fatal("correction changed")
	}
	gitCmd(t, f.sctx.WorkDir, "merge-base", "--is-ancestor", f.published, f.retained)
	body, err := os.ReadFile(f.bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	if parsePipelineAttestationForTest(t, string(body)).HeadSHA != f.retained {
		t.Fatal("attestation not rebound")
	}
	binding, err := f.sctx.DB.RetainedCIRepair(f.sctx.Run.ID)
	if err != nil || binding != nil {
		t.Fatalf("settled binding not cleared: %+v %v", binding, err)
	}
	commands, _ := os.ReadFile(f.logFile)
	edit, push := strings.LastIndex(string(commands), "\npr edit"), strings.Index(string(commands), "\npush ")
	if edit < 0 || push < edit {
		t.Fatalf("attestation not before push: %s", commands)
	}
}

func TestDLOCK31RetainedRetryRefusesDirtyOrChangedPolicy(t *testing.T) {
	for _, dirty := range []bool{true, false} {
		name := "trusted_revalidation_required"
		if dirty {
			name = "dirty_worktree"
		}
		t.Run(name, func(t *testing.T) {
			f := newDLOCK31Publication(t)
			if dirty {
				if err := os.WriteFile(filepath.Join(f.sctx.WorkDir, "untracked"), []byte("leave alone"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				f.sctx.Config.CI.RevalidateRepairs = true
			}
			outcome, err := (&CIStep{}).Execute(f.sctx)
			if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.RestartFrom != "" {
				t.Fatalf("unsafe retry: %+v %v", outcome, err)
			}
			remote, _ := os.ReadFile(f.remoteFile)
			if string(remote) != f.published || f.agentCalls != 0 {
				t.Fatal("retry pushed or invoked fixer")
			}
		})
	}
}

// Resume the persisted gate through the real executor and real CI publication,
// not the earlier admission sentinel. Only transport/provider/agent are fakes.
func TestDLOCK31FullResumePublishesRetainedRepair(t *testing.T) {
	f := newDLOCK31Publication(t)
	for _, err := range []error{
		f.sctx.DB.SetRunAwaitingAgent(f.sctx.Run.ID),
		f.sctx.DB.UpdateRunPRURL(f.sctx.Run.ID, *f.sctx.Run.PRURL),
		f.sctx.DB.UpdateStepStatusWithDuration(f.sctx.StepResultID, types.StepStatusFixReview, 1),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.sctx.DB.InsertStepRound(f.sctx.StepResultID, 1, "auto_fix", &f.sctx.PreviousFindings, nil, 1); err != nil {
		t.Fatal(err)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ci := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { cancel(); return ctx.Err() }}
	executor := pipeline.NewExecutor(f.sctx.DB, f.paths, f.sctx.Config, f.sctx.Agent, []pipeline.Step{ci}, nil)
	environment := runenv.Overlay{Set: map[string]string{}}
	for _, entry := range f.baseEnv {
		key, value, _ := strings.Cut(entry, "=")
		environment.Set[key] = value
		// exec.LookPath happens before Cmd.Env is applied during restoration.
		// Keep even that lookup inside the temporary fake-process directory.
		t.Setenv(key, value)
	}
	executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com", Environment: environment})
	done := make(chan error, 1)
	go func() { done <- executor.Resume(ctx, run, f.sctx.Repo, f.sctx.WorkDir) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := executor.Respond(types.StepCI, types.ActionFix, []string{"quota"}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restored CI gate never accepted response")
		}
		time.Sleep(time.Millisecond)
	}
	<-done // The test cancels only after publication, at the monitor poll seam.
	after, err := f.sctx.DB.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != run.ID || after.RepoID != run.RepoID || after.Branch != run.Branch || after.LastPushedSHA == nil || *after.LastPushedSHA != f.retained || after.PushGeneration == nil || *after.PushGeneration != 2 {
		t.Fatalf("resume lost identity or failed publication: %+v", after)
	}
	if f.agentCalls != 0 {
		t.Fatalf("resume invoked fixer %d times", f.agentCalls)
	}
	rounds, err := f.sctx.DB.GetRoundsByStep(f.sctx.StepResultID)
	if err != nil || len(rounds) != 1 || rounds[0].Round != 1 {
		t.Fatalf("round history lost: %+v %v", rounds, err)
	}
	if gitCmd(t, f.sctx.WorkDir, "rev-parse", "HEAD") != f.retained {
		t.Fatal("resume replaced correction")
	}
	body, err := os.ReadFile(f.bodyFile)
	if err != nil || parsePipelineAttestationForTest(t, string(body)).HeadSHA != f.retained {
		t.Fatal("resume did not settle attestation")
	}
}
