//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestConcurrentRepositoryExecutionContexts drives two registered repositories
// through one real candidate daemon. Exact fake gh/Git executables fail if any
// ambient credential override survives. A fetch barrier forces both runs to be
// live together; profile B then fails PR creation with a credential sentinel,
// proving that it neither falls through to the ambient tools nor poisons A.
func TestConcurrentRepositoryExecutionContexts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable scripts")
	}
	scenario := configurableFixCommitScenario(t)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	ctx := context.Background()
	root := filepath.Dir(h.AgentLog)
	barrierDir := filepath.Join(root, "context-barrier")
	if err := os.MkdirAll(barrierDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realGit, err := execLookPathWithoutDir("git", h.BinDir)
	if err != nil {
		t.Fatal(err)
	}

	// Enable one review fix so each pushed branch ends with a pipeline-created
	// commit whose selected author identity can be asserted.
	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	globalData, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatal(err)
	}
	globalData = bytes.Replace(globalData, []byte("  review: 0\n"), []byte("  review: 1\n"), 1)
	if err := os.WriteFile(globalConfig, globalData, 0o644); err != nil {
		t.Fatal(err)
	}

	profileA := contextFixture{
		name: "profile-a", login: "account-a", author: "Account A", email: "account-a@example.test",
		parentURL: "https://github.com/parent-a/project-a.git", forkURL: "https://github.com/account-a/project-a.git",
		upstream: h.UpstreamDir, fork: filepath.Join(root, "fork-a.git"),
		binDir: filepath.Join(root, "profile-a-bin"), configDir: filepath.Join(root, "profile-a-gh"), log: filepath.Join(root, "profile-a.log"),
	}
	profileB := contextFixture{
		name: "profile-b", login: "account-b", author: "Account B", email: "account-b@example.test",
		parentURL: "https://github.com/parent-b/project-b.git", forkURL: "https://github.com/account-b/project-b.git",
		upstream: filepath.Join(root, "upstream-b.git"), fork: filepath.Join(root, "fork-b.git"),
		binDir: filepath.Join(root, "profile-b-bin"), configDir: filepath.Join(root, "profile-b-gh"), log: filepath.Join(root, "profile-b.log"),
		failPRCreate: true,
	}

	initBareRemote(t, h, profileA.fork)
	if out, err := h.runGit(ctx, h.WorkDir, "push", profileA.fork, "main"); err != nil {
		t.Fatalf("seed fork A: %v\n%s", err, out)
	}
	workB := filepath.Join(root, "work-b")
	initSecondContextRepo(t, h, workB, profileB.upstream, profileB.fork)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", profileA.parentURL); err != nil {
		t.Fatalf("set profile A origin: %v\n%s", err, out)
	}

	writeContextFixture(t, profileA, realGit, barrierDir, profileB.parentURL, profileB.forkURL)
	writeContextFixture(t, profileB, realGit, barrierDir, profileA.parentURL, profileA.forkURL)
	contextA := writeContextDocument(t, profileA)
	contextB := writeContextDocument(t, profileB)

	if out, err := h.Run("init", "--fork-url", profileA.forkURL, "--github-context", contextA); err != nil {
		t.Fatalf("init profile A: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(workB, "init", "--fork-url", profileB.forkURL, "--github-context", contextB); err != nil {
		t.Fatalf("init profile B: %v\n%s", err, out)
	}

	branchA := "feature/context-a"
	branchB := "feature/context-b"
	h.CommitChange(branchA, "feature.txt", "unsafe A\n", "add profile A change")
	commitChangeInDir(t, h, workB, branchB, "feature.txt", "unsafe B\n")
	if err := os.WriteFile(filepath.Join(barrierDir, "armed"), []byte("armed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, push := range []struct{ dir, branch string }{{h.WorkDir, branchA}, {workB, branchB}} {
		push := push
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := h.runGit(context.Background(), push.dir, "push", "no-mistakes", push.branch)
			if err != nil {
				errs <- fmt.Errorf("push %s: %w: %s", push.branch, err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	waitForContextFixReviewAndApprove(t, h, h.WorkDir, branchA, 90*time.Second)
	waitForContextFixReviewAndApprove(t, h, workB, branchB, 90*time.Second)
	repoA, runA := waitForContextRun(t, h.NMHome, h.WorkDir, branchA, 120*time.Second)
	repoB, runB := waitForContextRun(t, h.NMHome, workB, branchB, 120*time.Second)
	if runA.Status != types.RunCompleted {
		t.Fatalf("profile A status=%s error=%v", runA.Status, runA.Error)
	}
	if runB.Status != types.RunFailed {
		t.Fatalf("profile B status=%s, want failed; error=%v", runB.Status, runB.Error)
	}
	if runB.Error != nil && strings.Contains(*runB.Error, "credential-sentinel") {
		t.Fatalf("profile B durable error leaked credential sentinel: %s", *runB.Error)
	}
	if runA.PRURL == nil || !strings.HasPrefix(*runA.PRURL, "https://github.com/parent-a/project-a/pull/") {
		t.Fatalf("profile A PR URL=%v", runA.PRURL)
	}
	if repoA.GitHubContext == nil || repoA.GitHubContext.ExpectedLogin != profileA.login || repoB.GitHubContext == nil || repoB.GitHubContext.ExpectedLogin != profileB.login {
		t.Fatalf("persisted contexts crossed: A=%#v B=%#v", repoA.GitHubContext, repoB.GitHubContext)
	}

	for _, profile := range []contextFixture{profileA, profileB} {
		got := strings.TrimSpace(string(mustGitOutput(t, h, profile.fork, "show", "-s", "--format=%an <%ae>", "refs/heads/"+map[string]string{profileA.name: branchA, profileB.name: branchB}[profile.name])))
		want := profile.author + " <" + profile.email + ">"
		if got != want {
			t.Errorf("%s fix commit identity=%q, want %q", profile.name, got, want)
		}
	}

	logA := readTextFile(t, profileA.log)
	logB := readTextFile(t, profileB.log)
	for _, assertion := range []struct {
		log, ownParent, ownHead, foreign, profile string
	}{
		{logA, "--repo parent-a/project-a", "--head account-a:" + branchA, profileB.parentURL, profileA.name},
		{logB, "--repo parent-b/project-b", "--head account-b:" + branchB, profileA.parentURL, profileB.name},
	} {
		if !strings.Contains(assertion.log, "git "+assertion.profile) || !strings.Contains(assertion.log, "gh "+assertion.profile) {
			t.Errorf("%s did not route both Git and GitHub subprocesses through its exact executables:\n%s", assertion.profile, assertion.log)
		}
		if !strings.Contains(assertion.log, assertion.ownParent) || !strings.Contains(assertion.log, assertion.ownHead) {
			t.Errorf("%s did not preserve parent/fork PR routing:\n%s", assertion.profile, assertion.log)
		}
		if strings.Contains(assertion.log, assertion.foreign) {
			t.Errorf("%s observed the other repository route:\n%s", assertion.profile, assertion.log)
		}
		if strings.Contains(assertion.log, "credential-sentinel") {
			t.Errorf("%s recorder leaked ambient sentinel", assertion.profile)
		}
	}
	if _, err := os.Stat(filepath.Join(barrierDir, "done")); err != nil {
		t.Fatalf("concurrent fetch barrier was not completed: %v", err)
	}

	// Scan every candidate-owned durable surface and recorder. The sentinel was
	// present in ambient token/config variables and profile B's failing stderr,
	// but must not survive in any file.
	for _, scanRoot := range []string{h.NMHome, h.AgentLog, h.WorkDir, workB, profileA.log, profileB.log} {
		if found := findBytes(t, scanRoot, []byte("credential-sentinel")); found != "" {
			t.Fatalf("credential sentinel persisted in %s", found)
		}
	}
}

type contextFixture struct {
	name, login, author, email string
	parentURL, forkURL         string
	upstream, fork             string
	binDir, configDir, log     string
	failPRCreate               bool
}

func writeContextFixture(t *testing.T, f contextFixture, realGit, barrierDir, foreignParent, foreignFork string) {
	t.Helper()
	for _, dir := range []string{f.binDir, f.configDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitScript := fmt.Sprintf(`#!/bin/bash
set -eu
if [[ -n "${GH_TOKEN+x}" || -n "${GITHUB_TOKEN+x}" || -n "${GIT_ASKPASS+x}" || -n "${GIT_SSH_COMMAND+x}" ]]; then
  printf 'credential-sentinel\n' >&2
  exit 90
fi
if [[ "${GH_CONFIG_DIR:-}" != %q ]]; then exit 91; fi
helper_ok=0
identity_ok=0
for ((i=0; i<${GIT_CONFIG_COUNT:-0}; i++)); do
  key_var="GIT_CONFIG_KEY_$i"
  value_var="GIT_CONFIG_VALUE_$i"
  key="${!key_var:-}"
  value="${!value_var:-}"
  if [[ "$key" = credential.helper && "$value" = *%q* ]]; then helper_ok=1; fi
  if [[ "$key" = user.name && "$value" = %q ]]; then identity_ok=1; fi
done
if [[ "$helper_ok" != 1 || "$identity_ok" != 1 ]]; then exit 95; fi
args=("$@")
network=0
for arg in "${args[@]}"; do
  case "$arg" in fetch|push|ls-remote) network=1 ;; esac
done
if [[ "$network" = 1 ]]; then
  for i in "${!args[@]}"; do
    case "${args[$i]}" in
      origin|%s) args[$i]=%q ;;
      %s) args[$i]=%q ;;
      %s|%s) printf 'foreign profile route refused\n' >&2; exit 92 ;;
    esac
  done
fi
printf 'git %s %%s\n' "${args[*]}" >> %q
if [[ -f %q && ! -f %q ]]; then
  for arg in "$@"; do
    if [[ "$arg" = fetch ]]; then
      touch %q
      deadline=$((SECONDS+20))
      while [[ ! -f %q ]]; do
        if (( SECONDS >= deadline )); then exit 93; fi
        sleep 0.05
      done
      touch %q
      break
    fi
  done
fi
exec %q "${args[@]}"
`, f.configDir, filepath.Join(f.binDir, "gh"), f.author, f.parentURL, f.upstream, f.forkURL, f.fork, foreignParent, foreignFork, f.name, f.log,
		filepath.Join(barrierDir, "armed"), filepath.Join(barrierDir, "done"), filepath.Join(barrierDir, f.name),
		filepath.Join(barrierDir, map[string]string{"profile-a": "profile-b", "profile-b": "profile-a"}[f.name]), filepath.Join(barrierDir, "done"), realGit)
	if err := os.WriteFile(filepath.Join(f.binDir, "git"), []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	failCreate := ""
	if f.failPRCreate {
		failCreate = "printf 'credential-sentinel\\n' >&2; exit 41"
	} else {
		failCreate = "printf 'https://github.com/parent-a/project-a/pull/101\\n'; exit 0"
	}
	ghScript := fmt.Sprintf(`#!/bin/bash
set -eu
if [[ -n "${GH_TOKEN+x}" || -n "${GITHUB_TOKEN+x}" || -n "${GIT_ASKPASS+x}" ]]; then
  printf 'credential-sentinel\n' >&2
  exit 90
fi
if [[ "${GH_CONFIG_DIR:-}" != %q || "${GH_HOST:-}" != github.com ]]; then exit 91; fi
printf 'gh %s %%s\n' "$*" >> %q
if [[ "$1" = auth && "$2" = status ]]; then exit 0; fi
if [[ "$1" = api ]]; then printf '%%s\n' %q; exit 0; fi
if [[ "$1" = repo && "$2" = view ]]; then printf 'WRITE\n'; exit 0; fi
if [[ "$1" = pr && "$2" = list ]]; then printf '[]\n'; exit 0; fi
if [[ "$1" = pr && "$2" = create ]]; then %s; fi
if [[ "$1" = pr && "$2" = view ]]; then printf 'CLOSED\n'; exit 0; fi
if [[ "$1" = pr && "$2" = checks ]]; then printf '[]\n'; exit 0; fi
if [[ "$1" = run ]]; then printf '[]\n'; exit 0; fi
exit 94
`, f.configDir, f.name, f.log, f.login, failCreate)
	if err := os.WriteFile(filepath.Join(f.binDir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeContextDocument(t *testing.T, f contextFixture) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(f.binDir), f.name+"-context.json")
	body := fmt.Sprintf(`{
  "version": 1,
  "gh_path": %q,
  "git_path": %q,
  "gh_config_dir": %q,
  "host": "github.com",
  "expected_login": %q,
  "git_protocol": "https",
  "credential_helper": "gh",
  "commit_author": {"name": %q, "email": %q},
  "label": %q
}
`, filepath.Join(f.binDir, "gh"), filepath.Join(f.binDir, "git"), f.configDir, f.login, f.author, f.email, f.name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func initBareRemote(t *testing.T, h *Harness, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(context.Background(), path, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init bare %s: %v\n%s", path, err, out)
	}
}

func initSecondContextRepo(t *testing.T, h *Harness, work, upstream, fork string) {
	t.Helper()
	initBareRemote(t, h, upstream)
	initBareRemote(t, h, fork)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"config", "user.name", "E2E Test"}, {"config", "user.email", "e2e@example.test"}, {"config", "commit.gpgsign", "false"}} {
		if out, err := h.runGit(context.Background(), work, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# profile B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte("allow_repo_commands: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "initial"}, {"remote", "add", "origin", upstream}, {"push", "-u", "origin", "main"}, {"push", fork, "main"}, {"remote", "set-url", "origin", "https://github.com/parent-b/project-b.git"}} {
		if out, err := h.runGit(context.Background(), work, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func commitChangeInDir(t *testing.T, h *Harness, work, branch, path, content string) {
	t.Helper()
	for _, args := range [][]string{{"checkout", "-b", branch, "main"}} {
		if out, err := h.runGit(context.Background(), work, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(work, path), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", path}, {"commit", "-m", "context change"}} {
		if out, err := h.runGit(context.Background(), work, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func waitForContextFixReviewAndApprove(t *testing.T, h *Harness, work, branch string, timeout time.Duration) {
	t.Helper()
	resolved, _ := filepath.EvalSymlinks(work)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		database, err := db.Open(paths.WithRoot(h.NMHome).DB())
		if err == nil {
			repo, _ := database.GetRepoByPath(resolved)
			if repo != nil {
				runs, _ := database.GetRunsByRepo(repo.ID)
				for _, run := range runs {
					if run.Branch != branch {
						continue
					}
					steps, _ := database.GetStepsByRun(run.ID)
					for _, step := range steps {
						if step.StepName == types.StepReview && step.Status == types.StepStatusFixReview {
							database.Close()
							h.Respond(run.ID, types.StepReview, types.ActionApprove)
							return
						}
					}
				}
			}
			database.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach review fix-review", branch)
}

func waitForContextRun(t *testing.T, nmHome, work, branch string, timeout time.Duration) (*db.Repo, *db.Run) {
	t.Helper()
	resolved, _ := filepath.EvalSymlinks(work)
	deadline := time.Now().Add(timeout)
	var last *db.Run
	for time.Now().Before(deadline) {
		database, err := db.Open(paths.WithRoot(nmHome).DB())
		if err == nil {
			repo, repoErr := database.GetRepoByPath(resolved)
			if repoErr == nil && repo != nil {
				runs, runErr := database.GetRunsByRepo(repo.ID)
				if runErr == nil {
					for _, run := range runs {
						if run.Branch == branch {
							last = run
							if run.Status == types.RunCompleted || run.Status == types.RunFailed || run.Status == types.RunCancelled {
								database.Close()
								return repo, run
							}
						}
					}
				}
			}
			database.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish; last=%#v", branch, last)
	return nil, nil
}

func mustGitOutput(t *testing.T, h *Harness, dir string, args ...string) []byte {
	t.Helper()
	out, err := h.runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func readTextFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func findBytes(t *testing.T, root string, needle []byte) string {
	t.Helper()
	info, err := os.Stat(root)
	if err != nil {
		return ""
	}
	if !info.IsDir() {
		data, _ := os.ReadFile(root)
		if bytes.Contains(data, needle) {
			return root
		}
		return ""
	}
	found := ""
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || found != "" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil && bytes.Contains(data, needle) {
			found = path
		}
		return nil
	})
	return found
}

func execLookPathWithoutDir(name, excludedDir string) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || filepath.Clean(dir) == filepath.Clean(excludedDir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found outside %s", name, excludedDir)
}
