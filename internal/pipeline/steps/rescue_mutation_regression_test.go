//go:build unix

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
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func retainMutationFixture(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	head, err := git.HeadSHA(context.Background(), sctx.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := sctx.DB.BeginWorkRescue(sctx.Run, "test", "", head, sctx.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	p.State = "retained"
	p.Reason = "original bytes required"
	if err := sctx.DB.SaveWorkRescue(p); err != nil {
		t.Fatal(err)
	}
}

func TestRescueMutationTestTimeoutRefusesCommandsAndCleanup(t *testing.T) {
	setRescueFixturePopulation(t)
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "fixture", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
		writeFixtureFile(t, dir, ".gitignore", "coverage/\n")
		if err := os.MkdirAll(filepath.Join(dir, "coverage"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, dir, "coverage/private", "interrupted coverage bytes\n")
		<-ctx.Done()
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Prepare: "echo prepare >> invocations", Test: "rm -rf coverage; echo test >> invocations"})
	sctx.GateDir = t.TempDir()
	gitCmd(t, sctx.GateDir, "init", "--bare")
	sctx.Shared = &pipeline.RunShared{}
	sctx.Fixing = true
	sctx.Config.TestAgentTimeout = 50 * time.Millisecond
	previous := runPreparationCleanup
	cleanups := 0
	runPreparationCleanup = func(ctx context.Context, dir, head string, modules []preparationSubmodule) error {
		cleanups++
		return previous(ctx, dir, head, modules)
	}
	t.Cleanup(func() { runPreparationCleanup = previous })
	outcome, err := (&TestStep{}).Execute(sctx)
	if !errors.Is(err, pipeline.ErrWorkRetained) || !errors.Is(err, errTestAgentTimeout) {
		t.Errorf("retained timeout precedence: outcome=%+v err=%v", outcome, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "coverage/private")); err != nil || string(got) != "interrupted coverage bytes\n" {
		t.Errorf("retained bytes=%q error=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "invocations")); !os.IsNotExist(err) || cleanups != 0 {
		t.Errorf("commands/cleanup invoked: marker=%v cleanups=%d", err, cleanups)
	}
	if gitCmd(t, dir, "rev-parse", "HEAD") != head {
		t.Fatal("timeout committed retained work")
	}
}

func TestRescueMutationSharedCommandAndStageBoundaries(t *testing.T) {
	for _, state := range []string{"retained", "unreadable"} {
		for _, consumer := range []string{"prepare", "format", "test", "lint", "additional", "stage", "custom"} {
			t.Run(state+"/"+consumer, func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				sctx := newTestContextWithDBRecords(t, &mockAgent{name: "fixture"}, dir, base, head, config.Commands{})
				writeFixtureFile(t, dir, "base.txt", "retained working bytes\n")
				retainMutationFixture(t, sctx)
				if state == "unreadable" {
					if err := sctx.DB.Close(); err != nil {
						t.Fatal(err)
					}
				}
				before := gitCmd(t, dir, "ls-files", "--stage")
				var err error
				if consumer == "stage" {
					err = stagePipelineChanges(sctx)
				} else if consumer == "custom" {
					_, _, err = runStepShellCommand(sctx, "echo ran > invocations; echo erased > base.txt")
				} else if consumer == "additional" {
					sctx.Config.CommandOverrides = map[string]config.CommandOverride{"test": {Additional: []string{"echo ran > invocations; echo erased > base.txt"}}}
					_, _, err = runConfiguredChecks(sctx, "test", "")
				} else {
					_, _, err = runRepositoryCommand(sctx, consumer, "echo ran > invocations; echo erased > base.txt")
				}
				if !errors.Is(err, pipeline.ErrWorkRetained) {
					t.Errorf("mutation boundary failed to refuse: %v", err)
				}
				if got, err := os.ReadFile(filepath.Join(dir, "base.txt")); err != nil || string(got) != "retained working bytes\n" {
					t.Errorf("original bytes=%q %v", got, err)
				}
				if _, err := os.Stat(filepath.Join(dir, "invocations")); !os.IsNotExist(err) {
					t.Error("command ran")
				}
				if got := gitCmd(t, dir, "ls-files", "--stage"); got != before {
					t.Error("retained index changed")
				}
			})
		}
	}
}

func TestRescueMutationPushRefusesBeforeFormatStageAndCommit(t *testing.T) {
	for _, format := range []string{"", "echo formatted > invocations; echo erased > base.txt"} {
		t.Run(map[bool]string{true: "format", false: "no-format"}[format != ""], func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "fixture"}, dir, base, head, config.Commands{Format: format})
			writeFixtureFile(t, dir, "base.txt", "retained working bytes\n")
			retainMutationFixture(t, sctx)
			before := gitCmd(t, dir, "ls-files", "--stage")
			if _, err := (&PushStep{}).Execute(sctx); !errors.Is(err, pipeline.ErrWorkRetained) {
				t.Errorf("Push refusal=%v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "invocations")); !os.IsNotExist(err) {
				t.Error("formatter ran")
			}
			if gitCmd(t, dir, "rev-parse", "HEAD") != head || gitCmd(t, dir, "ls-files", "--stage") != before {
				t.Error("Push committed or staged retained work")
			}
			if got, err := os.ReadFile(filepath.Join(dir, "base.txt")); err != nil || string(got) != "retained working bytes\n" {
				t.Errorf("Push lost bytes=%q %v", got, err)
			}
		})
	}
}

func TestRescueMutationPreparationRetentionPreservesSnapshot(t *testing.T) {
	for _, phase := range []string{"entry", "command-return", "cleanup-return"} {
		t.Run(phase, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "fixture"}, dir, base, head, config.Commands{Prepare: "echo prepare-complete"})
			sctx.GateDir = t.TempDir()
			gitCmd(t, sctx.GateDir, "init", "--bare")
			sctx.Shared = &pipeline.RunShared{}
			cleanups := 0
			previous := runPreparationCleanup
			runPreparationCleanup = func(ctx context.Context, dir, head string, modules []preparationSubmodule) error {
				cleanups++
				if phase == "cleanup-return" {
					writeFixtureFile(t, dir, "base.txt", "new retained bytes\n")
					retainMutationFixture(t, sctx)
				}
				return nil
			}
			t.Cleanup(func() { runPreparationCleanup = previous })
			if phase == "entry" {
				writeFixtureFile(t, dir, "base.txt", "new retained bytes\n")
				retainMutationFixture(t, sctx)
			}
			sctx.Log = func(line string) {
				if strings.TrimSpace(line) == "prepare-complete" && phase == "command-return" {
					writeFixtureFile(t, dir, "base.txt", "new retained bytes\n")
					retainMutationFixture(t, sctx)
				}
			}
			err := ensurePrepared(sctx, types.StepTest)
			if !errors.Is(err, pipeline.ErrWorkRetained) {
				t.Errorf("preparation refusal=%v", err)
			}
			if got, e := os.ReadFile(filepath.Join(dir, "base.txt")); e != nil || string(got) != "new retained bytes\n" {
				t.Errorf("cleanup/restore lost original bytes=%q %v", got, e)
			}
			if phase != "cleanup-return" && cleanups != 0 {
				t.Errorf("cleanup invoked %d times", cleanups)
			}
			if phase != "entry" {
				parent, e := preparationSnapshotParent(context.Background(), dir, sctx.GateDir)
				if e != nil {
					t.Fatal(e)
				}
				snapshots, e := filepath.Glob(filepath.Join(parent, "prepare-*"))
				if e != nil || len(snapshots) == 0 {
					t.Errorf("recovery snapshot removed: %v %v", snapshots, e)
				}
			}
		})
	}
}

func TestRescueMutationVerifiedTimeoutKeepsConfiguredFallback(t *testing.T) {
	setRescueFixturePopulation(t)
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "fixture", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
		<-ctx.Done()
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Test: "echo checked > command.invoked"})
	sctx.Fixing = true
	sctx.Config.TestAgentTimeout = 50 * time.Millisecond
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("verified clean timeout fallback: %+v %v", outcome, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "command.invoked")); err != nil || strings.TrimSpace(string(got)) != "checked" {
		t.Fatalf("configured fallback absent: %q %v", got, err)
	}
	if p, err := sctx.DB.LatestWorkRescue(sctx.Run.ID); err != nil || p != nil {
		t.Fatalf("clean invocation was retained: %+v %v", p, err)
	}
}
