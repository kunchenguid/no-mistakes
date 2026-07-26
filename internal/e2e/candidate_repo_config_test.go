//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRunCommandsComeFromCandidateRepository is the two-clone counterfactual
// for command resolution. The primary clone and candidate checkout share one
// upstream, but a run must execute only the command committed in its submitted
// candidate. Both linked and standalone candidate checkout paths are covered.
func TestRunCommandsComeFromCandidateRepository(t *testing.T) {
	for _, tc := range []struct {
		name       string
		standalone bool
	}{
		{name: "linked_worktree"},
		{name: "standalone_clone", standalone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			optOut := false
			h := NewHarness(t, SetupOpts{
				Agent:             "claude",
				Scenario:          cleanReviewScenario(t),
				AllowRepoCommands: &optOut,
			})

			staleMarker := filepath.Join(t.TempDir(), "stale-command")
			candidateMarker := filepath.Join(t.TempDir(), "candidate-command")
			candidateStarted := filepath.Join(t.TempDir(), "candidate-started")
			releaseCandidate := filepath.Join(t.TempDir(), "release-candidate")
			changedCloneMarker := filepath.Join(t.TempDir(), "changed-clone-command")
			staleConfig := fmt.Sprintf("allow_repo_commands: false\ncommands:\n  test: %q\n", "printf stale > "+shellQuote(staleMarker))
			h.CommitChange("main", ".no-mistakes.yaml", staleConfig, "configure primary sentinel")
			if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
				t.Fatalf("push primary config: %v\n%s", err, out)
			}
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init primary: %v\n%s", err, out)
			}

			branch := "candidate-" + tc.name
			candidateDir := h.WorkDir
			if tc.standalone {
				candidateDir = filepath.Join(t.TempDir(), "standalone")
				if out, err := h.runGit(context.Background(), filepath.Dir(candidateDir), "clone", h.UpstreamDir, candidateDir); err != nil {
					t.Fatalf("clone standalone candidate: %v\n%s", err, out)
				}
				for _, setting := range [][]string{
					{"config", "user.email", "e2e@example.com"},
					{"config", "user.name", "E2E Test"},
					{"config", "commit.gpgsign", "false"},
					{"checkout", "-b", branch},
				} {
					if out, err := h.runGit(context.Background(), candidateDir, setting...); err != nil {
						t.Fatalf("prepare standalone candidate with git %v: %v\n%s", setting, err, out)
					}
				}
				writeCandidateConfig(t, h, candidateDir, candidateMarker, candidateStarted, releaseCandidate)
			} else {
				h.CommitChange(branch, ".no-mistakes.yaml", candidateConfig(candidateMarker, candidateStarted, releaseCandidate), "configure candidate sentinel")
				candidateDir = h.AddWorktree(branch)
			}

			if out, err := h.RunInDir(candidateDir, "init"); err != nil {
				t.Fatalf("init candidate: %v\n%s", err, out)
			}
			pushCandidate(t, h, candidateDir, branch)
			waitForFile(t, candidateStarted, 10*time.Second)

			// Once the run has started its configured command, mutate the
			// other clone's config. The active run must keep the command
			// snapped from its immutable submitted SHA.
			changedCloneConfig := fmt.Sprintf("commands:\n  test: %q\n", "printf changed > "+shellQuote(changedCloneMarker))
			if err := os.WriteFile(filepath.Join(h.WorkDir, ".no-mistakes.yaml"), []byte(changedCloneConfig), 0o644); err != nil {
				t.Fatalf("change primary clone config after run creation: %v", err)
			}
			if err := os.WriteFile(releaseCandidate, []byte("release\n"), 0o644); err != nil {
				t.Fatalf("release candidate command: %v", err)
			}

			repoID := repoIDForPath(t, candidateDir)
			if !tc.standalone {
				repoID = h.repoID()
			}
			run := waitForCandidateRun(t, h, repoID, branch)
			if run.Status != types.RunCompleted {
				t.Fatalf("candidate run did not complete: status=%s error=%v", run.Status, deref(run.Error))
			}
			_, candidateErr := os.Stat(candidateMarker)
			_, staleErr := os.Stat(staleMarker)
			_, changedCloneErr := os.Stat(changedCloneMarker)
			if candidateErr != nil || !os.IsNotExist(staleErr) || !os.IsNotExist(changedCloneErr) {
				t.Fatalf("wrong command resolution: candidate stat=%v; stale primary stat=%v; changed clone stat=%v", candidateErr, staleErr, changedCloneErr)
			}
		})
	}
}

func candidateConfig(marker, started, release string) string {
	command := "printf started > " + shellQuote(started) +
		"; while [ ! -f " + shellQuote(release) +
		" ]; do sleep 0.05; done; printf candidate > " + shellQuote(marker)
	return fmt.Sprintf("commands:\n  test: %q\n", command)
}

func writeCandidateConfig(t *testing.T, h *Harness, dir, marker, started, release string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(candidateConfig(marker, started, release)), 0o644); err != nil {
		t.Fatalf("write standalone candidate config: %v", err)
	}
	for _, args := range [][]string{
		{"add", ".no-mistakes.yaml"},
		{"commit", "-m", "configure candidate sentinel"},
	} {
		if out, err := h.runGit(context.Background(), dir, args...); err != nil {
			t.Fatalf("commit standalone candidate with git %v: %v\n%s", args, err, out)
		}
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func pushCandidate(t *testing.T, h *Harness, dir, branch string) {
	t.Helper()
	const skipped = "intent,rebase,review,document,lint,push,pr,ci"
	if out, err := h.runGit(context.Background(), dir, "push", "-o", "no-mistakes.skip="+skipped, "no-mistakes", branch); err != nil {
		t.Fatalf("push candidate through gate: %v\n%s", err, out)
	}
}

func repoIDForPath(t *testing.T, dir string) string {
	t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("absolute candidate path: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:6])
}

func waitForCandidateRun(t *testing.T, h *Harness, repoID, branch string) *ipc.RunInfo {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var latest *ipc.RunInfo
	for time.Now().Before(deadline) {
		client, err := ipc.Dial(paths.WithRoot(h.NMHome).Socket())
		if err == nil {
			var result ipc.GetRunsResult
			callErr := client.Call(ipc.MethodGetRuns, &ipc.GetRunsParams{RepoID: repoID}, &result)
			_ = client.Close()
			if callErr == nil {
				for i := range result.Runs {
					if result.Runs[i].Branch != branch {
						continue
					}
					latest = &result.Runs[i]
					switch latest.Status {
					case types.RunCompleted, types.RunFailed, types.RunCancelled:
						return latest
					}
					break
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if latest != nil {
		t.Fatalf("candidate run did not finish (last status %s)", latest.Status)
	}
	t.Fatalf("candidate run did not appear")
	return nil
}
