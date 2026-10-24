//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This matrix seeds only the run records, not caller identity: every probe is
// a real shell and CLI connected to the harness daemon over authenticated IPC.
func TestGateRunContextIdentityMatrix(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	p := paths.WithRoot(h.NMHome)
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	mustGit := func(dir string, args ...string) {
		t.Helper()
		if out, err := h.runGit(context.Background(), dir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	otherOrigin := filepath.Join(t.TempDir(), "other.git")
	otherCopy := filepath.Join(t.TempDir(), "other")
	mustGit(h.WorkDir, "clone", "--bare", h.UpstreamDir, otherOrigin)
	mustGit(h.WorkDir, "clone", otherOrigin, otherCopy)
	if out, err := h.RunInDir(otherCopy, "init"); err != nil {
		t.Fatalf("init second repository: %v\n%s", err, out)
	}
	repos, err := d.GetRepos()
	if err != nil || len(repos) != 2 {
		t.Fatalf("repos: %v, %v", repos, err)
	}
	otherID := repos[0].ID
	if otherID == h.repoID() {
		otherID = repos[1].ID
	}
	phase := types.StepName("gate.test.scope")
	type fixture struct {
		run  *db.Run
		step *db.StepResult
		dir  string
	}
	seed := func(repo, branch string) fixture {
		t.Helper()
		run, err := d.InsertRun(repo, branch, "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "isolated")
		mustGit(p.RepoDir(repo), "--git-dir", p.RepoDir(repo), "fetch", "origin", "main")
		mustGit(p.RepoDir(repo), "--git-dir", p.RepoDir(repo), "worktree", "add", "--detach", dir, "FETCH_HEAD")
		if err := d.SetRunWorktreeDir(run.ID, dir); err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		step, err := d.InsertStepResult(run.ID, phase)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.StartStep(step.ID); err != nil {
			t.Fatal(err)
		}
		return fixture{run, step, dir}
	}
	a := seed(h.repoID(), "first")
	b := seed(h.repoID(), "concurrent")
	c := seed(otherID, "other-repository")
	bind := func(f fixture, pid *int) {
		t.Helper()
		if err := d.SetStepAgentActivity(f.step.ID, "fixture gate shell", pid); err != nil {
			t.Fatal(err)
		}
	}
	// Three live sibling roots: two runs in one repository and a third in
	// another repository. Neither CWD nor daemon ancestry can choose a run.
	var finish []func() gateContextOutput
	for _, f := range []fixture{a, b, c} {
		pid, collect := startGateContextProbe(t, h, f.dir)
		bind(f, &pid)
		finish = append(finish, collect)
	}
	for i, f := range []fixture{a, b, c} {
		got := finish[i]()
		assertAvailableGateContext(t, got, f.run.ID, f.run.RepoID, phase)
		canonical, err := filepath.EvalSymlinks(f.dir)
		if err != nil || got.Worktree != canonical {
			t.Fatalf("canonical copy = %q, want %q (%v)", got.Worktree, canonical, err)
		}
		bind(f, nil)
	}
	t.Run("symlink and subdirectory preserve canonical identity", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "linked-copy")
		if err := os.Symlink(a.dir, link); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(a.dir, "subdir"), 0o700); err != nil {
			t.Fatal(err)
		}
		pid, collect := startGateContextProbe(t, h, filepath.Join(link, "subdir"))
		bind(a, &pid)
		got := collect()
		assertAvailableGateContext(t, got, a.run.ID, a.run.RepoID, phase)
		canonical, err := filepath.EvalSymlinks(a.dir)
		if err != nil || got.Worktree != canonical {
			t.Fatalf("symlink changed copy identity: %+v, %v", got, err)
		}
		bind(a, nil)
	})
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"ordinary copy", h.WorkDir},
		{"other run copy", b.dir},
		{"other repository", c.dir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, collect := startGateContextProbe(t, h, tc.dir)
			bind(a, &pid)
			if got := collect(); got.Available || got.GateRunContext != (types.GateRunContext{}) {
				t.Fatalf("wrong-copy caller got context: %+v", got)
			}
			bind(a, nil)
		})
	}
	t.Run("missing ownership and forged ambient marker", func(t *testing.T) {
		_, collect := startGateContextProbe(t, h, a.dir)
		if got := collect(); got.Available {
			t.Fatalf("marker or topology authenticated caller: %+v", got)
		}
	})
	t.Run("ambiguous ancestry", func(t *testing.T) {
		pid, collect := startGateContextProbe(t, h, a.dir)
		bind(a, &pid)
		bind(b, &pid)
		if got := collect(); got.Available {
			t.Fatalf("shared process chose a run: %+v", got)
		}
		bind(a, nil)
		bind(b, nil)
	})
	for _, status := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview, types.StepStatusCompleted} {
		t.Run("inactive step "+string(status), func(t *testing.T) {
			pid, collect := startGateContextProbe(t, h, a.dir)
			bind(a, &pid)
			if err := d.UpdateStepStatus(a.step.ID, status); err != nil {
				t.Fatal(err)
			}
			if got := collect(); got.Available {
				t.Fatalf("inactive step exposed context: %+v", got)
			}
			if err := d.UpdateStepStatus(a.step.ID, types.StepStatusRunning); err != nil {
				t.Fatal(err)
			}
			bind(a, nil)
		})
	}
	t.Run("a core-step descendant is not a custom gate", func(t *testing.T) {
		pid, collect := startGateContextProbe(t, h, a.dir)
		step, err := d.InsertStepResult(a.run.ID, types.StepTest)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.StartStep(step.ID); err != nil {
			t.Fatal(err)
		}
		if err := d.SetStepAgentActivity(step.ID, "core step fixture", &pid); err != nil {
			t.Fatal(err)
		}
		if got := collect(); got.Available {
			t.Fatalf("core step exposed custom-gate context: %+v", got)
		}
		if err := d.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("terminal run", func(t *testing.T) {
		pid, collect := startGateContextProbe(t, h, a.dir)
		bind(a, &pid)
		if err := d.UpdateRunStatus(a.run.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
		if got := collect(); got.Available {
			t.Fatalf("terminal run exposed context: %+v", got)
		}
	})
}

func startGateContextProbe(t *testing.T, h *Harness, dir string) (int, func() gateContextOutput) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, "sh", "-c", `read -r ready || exit 9; "$@"; result=$?; exit "$result"`, "context-probe", h.NMBin, "axi", "context", "--json")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NO_MISTAKES_GATE=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		cancel()
		_ = input.Close()
		if !finished {
			_ = cmd.Wait()
		}
	})
	return cmd.Process.Pid, func() gateContextOutput {
		t.Helper()
		if _, err := input.Write([]byte("\n")); err != nil {
			t.Fatal(err)
		}
		_ = input.Close()
		err := cmd.Wait()
		finished = true
		var got gateContextOutput
		if decodeErr := json.Unmarshal(out.Bytes(), &got); decodeErr != nil {
			t.Fatalf("CLI context %q: %v (%v)", out.String(), decodeErr, err)
		}
		if got.Available != (err == nil) {
			t.Fatalf("exit status disagrees with context: %v, %+v", err, got)
		}
		if !got.Available {
			assertUnavailableContext(t, out.String())
		}
		return got
	}
}
