//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// E2ERepo is a second (or third, ...) gated repo registered under the same
// NM_HOME / daemon as the primary harness. It has its own bare upstream,
// working clone, and gate, so tests that need more than one repo (e.g. one
// shared profile applied across repos) can drive each independently. It reuses
// the harness's process-wide env (NM_HOME, PATH, HOME), so `nm` commands run in
// its working dir talk to the same daemon.
type E2ERepo struct {
	h           *Harness
	Name        string
	WorkDir     string
	UpstreamDir string
}

// NewRepo creates and registers an additional gated repo named name. repoConfig
// is the full body of the default-branch .no-mistakes.yaml (the caller controls
// allow_repo_commands and the profile: selection). extraFiles are additional
// default-branch files keyed by repo-relative path.
func (h *Harness) NewRepo(name, repoConfig string, extraFiles map[string]string) *E2ERepo {
	h.t.Helper()
	ctx := context.Background()
	root := filepath.Dir(h.WorkDir)
	r := &E2ERepo{
		h:           h,
		Name:        name,
		WorkDir:     filepath.Join(root, name),
		UpstreamDir: filepath.Join(root, name+".git"),
	}
	mustGit := func(dir string, args ...string) {
		out, err := h.runGit(ctx, dir, args...)
		if err != nil {
			h.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	if err := os.MkdirAll(r.WorkDir, 0o755); err != nil {
		h.t.Fatalf("mkdir repo %s: %v", name, err)
	}
	if err := os.MkdirAll(r.UpstreamDir, 0o755); err != nil {
		h.t.Fatalf("mkdir upstream %s: %v", name, err)
	}
	mustGit(r.UpstreamDir, "init", "--bare", "--initial-branch=main")

	mustGit(r.WorkDir, "init", "--initial-branch=main")
	mustGit(r.WorkDir, "config", "user.email", "e2e@example.com")
	mustGit(r.WorkDir, "config", "user.name", "E2E Test")
	mustGit(r.WorkDir, "config", "commit.gpgsign", "false")

	if err := os.WriteFile(filepath.Join(r.WorkDir, "README.md"), []byte("# "+name+"\n"), 0o644); err != nil {
		h.t.Fatalf("write readme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.WorkDir, ".no-mistakes.yaml"), []byte(repoConfig), 0o644); err != nil {
		h.t.Fatalf("write repo config: %v", err)
	}
	mustGit(r.WorkDir, "add", "README.md", ".no-mistakes.yaml")
	for path, content := range extraFiles {
		full := filepath.Join(r.WorkDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			h.t.Fatalf("mkdir for extra file %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			h.t.Fatalf("write extra file %s: %v", path, err)
		}
		mustGit(r.WorkDir, "add", path)
	}
	mustGit(r.WorkDir, "commit", "-m", "initial commit")
	mustGit(r.WorkDir, "remote", "add", "origin", r.UpstreamDir)
	mustGit(r.WorkDir, "push", "-u", "origin", "main")

	if out, err := h.RunInDir(r.WorkDir, "init"); err != nil {
		h.t.Fatalf("nm init in repo %s: %v\n%s", name, err, out)
	}
	return r
}

// CommitChange checks out (or creates) branch in the repo and commits a file.
func (r *E2ERepo) CommitChange(branch, path, content, message string) string {
	r.h.t.Helper()
	ctx := context.Background()
	current, err := r.h.runGit(ctx, r.WorkDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		r.h.t.Fatalf("rev-parse HEAD: %v", err)
	}
	if string(bytes.TrimSpace(current)) != branch {
		if _, err := r.h.runGit(ctx, r.WorkDir, "checkout", branch); err != nil {
			if _, err := r.h.runGit(ctx, r.WorkDir, "checkout", "-b", branch, "main"); err != nil {
				r.h.t.Fatalf("checkout %s: %v", branch, err)
			}
		}
	}
	full := filepath.Join(r.WorkDir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.h.t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.h.t.Fatalf("write %s: %v", path, err)
	}
	if _, err := r.h.runGit(ctx, r.WorkDir, "add", path); err != nil {
		r.h.t.Fatalf("git add %s: %v", path, err)
	}
	if _, err := r.h.runGit(ctx, r.WorkDir, "commit", "-m", message); err != nil {
		r.h.t.Fatalf("git commit: %v", err)
	}
	sha, err := r.h.runGit(ctx, r.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		r.h.t.Fatalf("rev-parse HEAD post-commit: %v", err)
	}
	return string(bytes.TrimSpace(sha))
}

// PushToGate pushes the current branch through the no-mistakes gate remote.
func (r *E2ERepo) PushToGate(branch string) {
	r.h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := r.h.runGit(ctx, r.WorkDir, "push", "no-mistakes", branch); err != nil {
		r.h.t.Fatalf("git push no-mistakes %s: %v\n%s", branch, err, out)
	}
}

// repoID mirrors gate.repoID for this repo's working path.
func (r *E2ERepo) repoID() string {
	abs := r.WorkDir
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:6])
}

// WaitForRun polls the daemon until a run for branch reaches a terminal state.
func (r *E2ERepo) WaitForRun(branch string, timeout time.Duration) *ipc.RunInfo {
	r.h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last *ipc.RunInfo
	for time.Now().Before(deadline) {
		runs := r.runs()
		for i := range runs {
			run := &runs[i]
			if run.Branch != branch {
				continue
			}
			last = run
			switch run.Status {
			case types.RunCompleted, types.RunFailed, types.RunCancelled:
				return run
			}
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if last != nil {
		r.h.t.Fatalf("repo %s: run for branch %s did not finish in %v (last status=%s)", r.Name, branch, timeout, last.Status)
	}
	r.h.t.Fatalf("repo %s: no run found for branch %s within %v", r.Name, branch, timeout)
	return nil
}

func (r *E2ERepo) runs() []ipc.RunInfo {
	p := paths.WithRoot(r.h.NMHome)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return nil
	}
	defer client.Close()
	var result ipc.GetRunsResult
	if err := client.Call(ipc.MethodGetRuns, &ipc.GetRunsParams{RepoID: r.repoID()}, &result); err != nil {
		return nil
	}
	return result.Runs
}
