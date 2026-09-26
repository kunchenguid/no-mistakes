package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// This fixture makes no live-product claim: it tests setup, not a user journey.
const preparationNoSurface = `{"findings":[],"summary":"setup probe","tested":["inspected setup marker"],"testing_summary":"No live product journey in this fixture","artifacts":[],"scenarios":[{"name":"setup probe","result":"untested","live":false,"evidence":"","reason":"step-boundary fixture only"}],"verdict":"no-surface"}`

func TestAgentOnlyPreparation_SharedAndRecovered(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		name := "default-no-surface"
		if optIn {
			name = "opted-in-no-surface"
		}
		t.Run(name, func(t *testing.T) {
			// Each new worktree must materialize independently, never a global receipt.
			for worktree := 0; worktree < 2; worktree++ {
				dir, base, head := setupGitRepo(t)
				ignoreTestDependencies(t, dir)
				calls := 0
				ag := &mockAgent{name: "prepare", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
					calls++
					_, err := os.Stat(filepath.Join(opts.CWD, ".deps", "count"))
					if (err == nil) != optIn {
						t.Fatalf("dependencies at agent entry: %v, opt-in=%v", err, optIn)
					}
					return &agent.Result{Output: json.RawMessage(preparationNoSurface)}, nil
				}}
				sctx := newPreparationTestContext(t, ag, dir, base, head, config.Commands{Prepare: preparationCommand(), Lint: dependencyExistsCommand()})
				sctx.Config.Test.Prepare = optIn
				sctx.Shared = &pipeline.RunShared{}
				var logs []string
				sctx.Log = func(line string) { logs = append(logs, line) }
				for attempt := 0; attempt < 3; attempt++ {
					if attempt == 2 {
						sctx.Shared = &pipeline.RunShared{}
					} // daemon recovery
					out, err := (&TestStep{}).Execute(sctx)
					if err != nil {
						t.Fatal(err)
					}
					if !out.NeedsApproval {
						t.Fatal("no-surface must still require a decision")
					}
				}
				if calls != 3 {
					t.Fatalf("fresh evidence calls=%d, want 3", calls)
				}
				sctx.Shared = &pipeline.RunShared{}
				out, err := (&LintStep{}).Execute(sctx)
				if err != nil || out.ExitCode != 0 {
					t.Fatalf("lint: %v, %+v", err, out)
				}
				if got := strings.Count(readFile(t, filepath.Join(dir, ".deps", "count")), "prepared"); got != 1 {
					t.Fatalf("materializations=%d, want 1", got)
				}
				joined := strings.Join(logs, "\n")
				if !strings.Contains(joined, preparationCommand()) || !strings.Contains(joined, "dependency preparation attempt completed in") {
					t.Fatalf("missing command/duration: %s", joined)
				}
				if got := readFile(t, filepath.Join(dir, "base.txt")); got != "base content" {
					t.Fatalf("setup mutation survived: %q", got)
				}
				if _, err := os.Stat(filepath.Join(dir, "prepare.tmp")); !os.IsNotExist(err) {
					t.Fatalf("setup artifact survived: %v", err)
				}
			}
		})
	}
}

func TestAgentOnlyPreparation_BeforeRepair(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	calls := 0
	ag := &mockAgent{name: "prepare", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if _, err := os.Stat(filepath.Join(opts.CWD, ".deps", "count")); err != nil {
			t.Fatalf("dependencies unavailable on turn %d: %v", calls, err)
		}
		if calls == 1 {
			return &agent.Result{Output: json.RawMessage(`{"summary":"No changes needed"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(preparationNoSurface)}, nil
	}}
	sctx := newPreparationTestContext(t, ag, dir, base, head, config.Commands{Prepare: preparationCommand()})
	sctx.Config.Test.Prepare = true
	sctx.Fixing = true
	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("repair + evidence calls=%d, want 2", calls)
	}
}

func TestAgentOnlyPreparation_RestorationFailureRetainsSnapshot(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	ag := &mockAgent{name: "prepare", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("agent launched despite restoration failure")
		return nil, nil
	}}
	sctx := newPreparationTestContext(t, ag, dir, base, head, config.Commands{Prepare: preparationCommand()})
	sctx.Config.Test.Prepare = true
	previous := runPreparationCleanup
	t.Cleanup(func() { runPreparationCleanup = previous })
	var snapshotDir string
	runPreparationCleanup = func(ctx context.Context, workDir, head string, submodules []preparationSubmodule) error {
		if err := cleanupPreparationChanges(ctx, workDir, head, submodules); err != nil {
			return err
		}
		indexes, err := filepath.Glob(filepath.Join(sctx.GateDir, "no-mistakes-preparation", "prepare-*", "root", "index"))
		if err != nil || len(indexes) != 1 {
			t.Fatalf("snapshot indexes=%v, err=%v", indexes, err)
		}
		snapshotDir = filepath.Dir(filepath.Dir(indexes[0]))
		snapshotDir, err = filepath.EvalSymlinks(snapshotDir)
		if err != nil {
			t.Fatalf("resolve snapshot directory: %v", err)
		}
		return os.Remove(indexes[0]) // simulate an unreadable restoration input
	}
	_, err := (&TestStep{}).Execute(sctx)
	if err == nil || snapshotDir == "" || !strings.Contains(err.Error(), "recovery snapshot retained at "+snapshotDir) {
		t.Fatalf("expected actionable restoration failure, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshotDir, "root", "unstaged.patch")); err != nil {
		t.Fatalf("recovery data lost: %v", err)
	}
	marker, err := preparationMarkerPath(sctx.Ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("restoration failure got receipt: %v", err)
	}
}

func TestAgentOnlyPreparation_FailureAndCancellationRestorePendingWork(t *testing.T) {
	for _, cancelCommand := range []bool{false, true} {
		name := "failure"
		if cancelCommand {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ignoreTestDependencies(t, dir)
			if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("staged\n"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "base.txt")
			if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("unstaged\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "pending.txt"), []byte("keep me"), 0600); err != nil {
				t.Fatal(err)
			}
			before := gitStatusPorcelain(t, dir)
			command := preparationCommand() + " && echo setup-output && " + failingPreparationCommand()
			if cancelCommand {
				command = preparationCommand() + " && echo setup-output && sleep 60"
				if runtime.GOOS == "windows" {
					command = preparationCommand() + " & echo setup-output & ping -n 60 127.0.0.1 >nul"
				}
			}
			calls := 0
			ag := &mockAgent{name: "prepare", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return &agent.Result{Output: json.RawMessage(preparationNoSurface)}, nil
			}}
			sctx := newPreparationTestContext(t, ag, dir, base, head, config.Commands{Prepare: command})
			sctx.Config.Test.Prepare = true
			sctx.Shared = &pipeline.RunShared{}
			var logs []string
			sctx.Log = func(line string) { logs = append(logs, line) }
			if cancelCommand {
				ctx, cancel := context.WithTimeout(sctx.Ctx, 30*time.Second)
				defer cancel()
				sctx.Ctx = ctx
				done := make(chan struct{})
				defer close(done)
				go func() {
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-done:
							return
						case <-ctx.Done():
							return
						case <-ticker.C:
							if _, err := os.Stat(filepath.Join(dir, "prepare.tmp")); err == nil {
								cancel()
								return
							}
						}
					}
				}()
			}
			if _, err := (&TestStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "prepare test dependencies") {
				t.Fatalf("expected preparation failure, got %v", err)
			}
			if calls != 0 {
				t.Fatal("agent launched after failed setup")
			}
			if got := gitStatusPorcelain(t, dir); got != before {
				t.Fatalf("status=%q, want %q", got, before)
			}
			if got := gitCmd(t, dir, "show", ":base.txt"); got != "staged" {
				t.Fatalf("index=%q", got)
			}
			if got := strings.ReplaceAll(readFile(t, filepath.Join(dir, "base.txt")), "\r\n", "\n"); got != "unstaged\n" {
				t.Fatalf("worktree=%q", got)
			}
			if got := readFile(t, filepath.Join(dir, "pending.txt")); got != "keep me" {
				t.Fatalf("untracked=%q", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "prepare.tmp")); !os.IsNotExist(err) {
				t.Fatalf("setup artifact survived: %v", err)
			}
			marker, err := preparationMarkerPath(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("failed setup got receipt: %v", err)
			}
			joined := strings.Join(logs, "\n")
			if !strings.Contains(joined, "dependency preparation attempt completed in") {
				t.Fatalf("missing failure duration: %s", joined)
			}
			if !cancelCommand && !strings.Contains(joined, "setup-output") {
				t.Fatalf("missing setup output: %s", joined)
			}
			// Neither ignored files left by failed setup nor in-memory state count as success.
			sctx.Ctx = context.Background()
			sctx.Config.Commands.Prepare = preparationCommand()
			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("retry evidence calls=%d", calls)
			}
			if got := strings.Count(readFile(t, filepath.Join(dir, ".deps", "count")), "prepared"); got != 2 {
				t.Fatalf("materializations=%d, want 2", got)
			}
		})
	}
}
