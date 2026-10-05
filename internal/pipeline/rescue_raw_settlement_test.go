//go:build unix

package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestRescueRawSettlementNormalizedBytes(t *testing.T) {
	for _, entry := range []string{"stopped", "cleanup", "saved"} {
		t.Run(entry, func(t *testing.T) {
			setRescueFixturePopulation(t)
			d, _, run, repo := setupTest(t)
			dir := t.TempDir()
			initGitRepo(t, dir)
			head, err := git.HeadSHA(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			run.HeadSHA = head
			if err := d.UpdateRunHeadSHA(run.ID, head); err != nil {
				t.Fatal(err)
			}
			attributes := filepath.Join(t.TempDir(), "attributes")
			if err := os.WriteFile(attributes, []byte("* text eol=crlf\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			execGit(t, dir, "config", "core.attributesfile", attributes)
			if entry == "saved" {
				if err := os.WriteFile(filepath.Join(dir, "partial"), []byte("already saved\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				p, err := d.BeginWorkRescue(run, "test", "", head, dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := PreserveRunWork(context.Background(), d, run, dir, p, "first stop", true); err != nil {
					t.Fatal(err)
				}
			}
			write := func() {
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\r\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				execGit(t, dir, "add", "README.md")
				status, err := git.RunWithEnv(context.Background(), dir, []string{"GIT_OPTIONAL_LOCKS=1"}, "status", "--porcelain")
				if err != nil || (entry != "saved" && status != "") {
					diff, _ := git.Run(context.Background(), dir, "diff", "--", "README.md")
					t.Fatalf("normalization fixture status=%q error=%v diff=%s", status, err, diff)
				}
			}
			if entry == "stopped" {
				ag := &hangingAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
					opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
					write()
					opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
					return nil, context.DeadlineExceeded
				}}
				sctx := &StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, Agent: ag, WorkDir: dir, Config: &config.Config{}}
				if _, err := sctx.RunAgent(agent.RunOpts{CWD: dir, Purpose: "test-fix"}); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("stop lost: %v", err)
				}
			} else {
				write()
				if err := PreserveRunWork(context.Background(), d, run, dir, nil, "cleanup", true); err != nil {
					t.Fatal(err)
				}
			}
			p, err := d.LatestWorkRescue(run.ID)
			if err != nil || p == nil || p.State != "saved" {
				t.Fatalf("normalized raw bytes not saved: %+v %v", p, err)
			}
			got, err := git.RunRaw(context.Background(), dir, "cat-file", "blob", p.Ref+":README.md")
			if err != nil || string(got) != "# test\r\n" {
				t.Fatalf("saved raw bytes=%q %v", got, err)
			}
		})
	}
}

func TestRescueRawSettlementSavedRecordCannotHideRewrittenHead(t *testing.T) {
	setRescueFixturePopulation(t)
	d, _, run, _ := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	if err := d.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "partial"), []byte("saved bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	original, err := d.BeginWorkRescue(run, "test", "", head, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := PreserveRunWork(context.Background(), d, run, dir, original, "first stop", true); err != nil {
		t.Fatal(err)
	}
	execGit(t, dir, "commit", "--amend", "--no-edit", "--allow-empty", "-m", "rewritten parent")
	err = PreserveRunWork(context.Background(), d, run, dir, nil, "cleanup", true)
	p, readErr := d.LatestWorkRescue(run.ID)
	if err == nil || readErr != nil || p == nil || p.State != "retained" {
		t.Fatalf("rewritten saved-parent continuity accepted: %+v err=%v read=%v", p, err, readErr)
	}
	if got, err := git.RunRaw(context.Background(), dir, "cat-file", "blob", original.Ref+":partial"); err != nil || string(got) != "saved bytes\n" {
		t.Fatal("original immutable rescue changed")
	}
}
