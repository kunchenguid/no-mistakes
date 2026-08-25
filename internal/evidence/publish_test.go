package evidence

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Ambient GIT_CONFIG_* injection from agent harnesses would leak into every
// git call these tests make, so drop it for the package.
func TestMain(m *testing.M) {
	os.Unsetenv("GIT_CONFIG_COUNT")
	os.Exit(m.Run())
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func runGitFails(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git %s unexpectedly succeeded: %s", strings.Join(args, " "), out)
	}
}

// newRepoWithRemote builds a bare remote plus a working clone holding one
// commit on main, which is what a repository looks like when a run starts.
func newRepoWithRemote(t *testing.T) (remote, work string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	work = filepath.Join(root, "work")

	runGit(t, root, "init", "--bare", "--initial-branch=main", remote)
	// Pin hooks to the repo's own dir: an ambient global core.hooksPath
	// would silently bypass hooks installed into the bare remote below.
	runGit(t, remote, "config", "core.hooksPath", "hooks")
	runGit(t, root, "init", "--initial-branch=main", work)
	runGit(t, work, "config", "user.name", "Evidence Test")
	runGit(t, work, "config", "user.email", "evidence@example.com")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("code\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "initial")
	runGit(t, work, "remote", "add", "origin", remote)
	runGit(t, work, "push", "-u", "origin", "main")
	return remote, work
}

func writeEvidence(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

func baseRequest(remote, work, source string) Request {
	return Request{
		RepoDir:   work,
		PushURL:   remote,
		Dir:       ".no-mistakes/evidence",
		Segments:  []string{"fm", "add-login"},
		SourceDir: source,
		Message:   "no-mistakes: evidence for fm/add-login",
	}
}

func TestPublish_LandsEvidenceOnOrphanBranchAndLeavesCodeBranchesUntouched(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	source := writeEvidence(t, t.TempDir(), map[string]string{
		"checkout.png":     "\x89PNG binary bytes\x00\x01",
		"logs/cli-run.txt": "it works\n",
	})
	mainBefore := runGit(t, remote, "rev-parse", "refs/heads/main")
	headBefore := runGit(t, work, "rev-parse", "HEAD")

	result, err := Publish(context.Background(), baseRequest(remote, work, source))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if result == nil {
		t.Fatal("Publish returned no result")
	}
	if result.Branch != DefaultBranch {
		t.Errorf("branch = %q, want default %q", result.Branch, DefaultBranch)
	}

	tip := runGit(t, remote, "rev-parse", "refs/heads/"+DefaultBranch)
	if tip != result.CommitSHA {
		t.Errorf("remote tip %s != published commit %s", tip, result.CommitSHA)
	}

	// The evidence branch carries the artifacts, byte for byte.
	tree := runGit(t, remote, "ls-tree", "-r", "--name-only", tip)
	for _, want := range []string{
		MarkerPath,
		".no-mistakes/evidence/fm/add-login/checkout.png",
		".no-mistakes/evidence/fm/add-login/logs/cli-run.txt",
	} {
		if !strings.Contains(tree, want) {
			t.Errorf("evidence branch is missing %q, has:\n%s", want, tree)
		}
	}
	if got := runGit(t, remote, "cat-file", "-p", tip+":.no-mistakes/evidence/fm/add-login/logs/cli-run.txt"); got != "it works" {
		t.Errorf("published content = %q", got)
	}

	// It is an orphan branch: no parents, and no history shared with main.
	if parents := runGit(t, remote, "rev-list", "--parents", "-n", "1", tip); parents != tip {
		t.Errorf("evidence commit has parents (%q), want an orphan root", parents)
	}
	runGitFails(t, remote, "merge-base", "refs/heads/main", tip)

	// Nothing reached the code branches or the caller's worktree.
	if after := runGit(t, remote, "rev-parse", "refs/heads/main"); after != mainBefore {
		t.Errorf("main moved from %s to %s", mainBefore, after)
	}
	if mainTree := runGit(t, remote, "ls-tree", "-r", "--name-only", "refs/heads/main"); strings.Contains(mainTree, "evidence") {
		t.Errorf("main carries evidence files:\n%s", mainTree)
	}
	if after := runGit(t, work, "rev-parse", "HEAD"); after != headBefore {
		t.Errorf("worktree HEAD moved from %s to %s", headBefore, after)
	}
	if status := runGit(t, work, "status", "--porcelain"); status != "" {
		t.Errorf("worktree is dirty after publishing:\n%s", status)
	}
}

func TestPublish_UsesConfiguredBranchName(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	source := writeEvidence(t, t.TempDir(), map[string]string{"proof.txt": "ok\n"})

	req := baseRequest(remote, work, source)
	req.Branch = "team/ci/evidence"
	result, err := Publish(context.Background(), req)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if result.Branch != "team/ci/evidence" {
		t.Errorf("branch = %q, want team/ci/evidence", result.Branch)
	}
	if got := runGit(t, remote, "rev-parse", "refs/heads/team/ci/evidence"); got != result.CommitSHA {
		t.Errorf("configured branch tip = %s, want %s", got, result.CommitSHA)
	}
	runGitFails(t, remote, "rev-parse", "--verify", "refs/heads/"+DefaultBranch)
}

func TestPublish_RejectsInvalidBranchName(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	source := writeEvidence(t, t.TempDir(), map[string]string{"proof.txt": "ok\n"})

	req := baseRequest(remote, work, source)
	req.Branch = "evidence branch"
	if _, err := Publish(context.Background(), req); err == nil {
		t.Fatal("Publish accepted an invalid branch name")
	} else if !strings.Contains(err.Error(), "invalid evidence branch name") {
		t.Errorf("error %q does not explain the invalid name", err)
	}
	if refs := runGit(t, remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Errorf("remote refs changed: %q", refs)
	}
}

func TestPublish_AppendsWithoutRewritingEarlierEvidence(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	first := writeEvidence(t, t.TempDir(), map[string]string{"round-1.txt": "first\n"})

	one, err := Publish(context.Background(), baseRequest(remote, work, first))
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}

	second := writeEvidence(t, t.TempDir(), map[string]string{"round-2.txt": "second\n"})
	two, err := Publish(context.Background(), baseRequest(remote, work, second))
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}

	if parent := runGit(t, remote, "rev-parse", two.CommitSHA+"^"); parent != one.CommitSHA {
		t.Errorf("second commit parent = %s, want %s (fast-forward append)", parent, one.CommitSHA)
	}
	// The first run's link target still resolves to the bytes it published.
	if got := runGit(t, remote, "cat-file", "-p", one.CommitSHA+":.no-mistakes/evidence/fm/add-login/round-1.txt"); got != "first" {
		t.Errorf("earlier evidence commit no longer resolves: %q", got)
	}
	if got := runGit(t, remote, "cat-file", "-p", two.CommitSHA+":.no-mistakes/evidence/fm/add-login/round-2.txt"); got != "second" {
		t.Errorf("later evidence missing: %q", got)
	}
}

func TestPublish_UnchangedEvidenceReusesTheExistingCommit(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	source := writeEvidence(t, t.TempDir(), map[string]string{"proof.txt": "ok\n"})

	one, err := Publish(context.Background(), baseRequest(remote, work, source))
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	two, err := Publish(context.Background(), baseRequest(remote, work, source))
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if one.CommitSHA != two.CommitSHA {
		t.Errorf("republishing identical evidence created %s, want the existing %s", two.CommitSHA, one.CommitSHA)
	}
}

func TestPublish_RefusesExistingBranchThatIsNotAnEvidenceBranch(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	source := writeEvidence(t, t.TempDir(), map[string]string{"proof.txt": "ok\n"})
	// Someone already uses this branch name for code.
	runGit(t, work, "push", "origin", "main:refs/heads/no-mistakes/evidence")
	before := runGit(t, remote, "rev-parse", "refs/heads/"+DefaultBranch)

	if _, err := Publish(context.Background(), baseRequest(remote, work, source)); err == nil {
		t.Fatal("Publish appended to a branch that is not an evidence branch")
	} else if !strings.Contains(err.Error(), MarkerPath) {
		t.Errorf("error %q does not explain the missing marker", err)
	}
	if after := runGit(t, remote, "rev-parse", "refs/heads/"+DefaultBranch); after != before {
		t.Errorf("pre-existing branch moved from %s to %s", before, after)
	}
}

func TestPublish_RefusesExistingBranchWithWrongMarkerContent(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	source := writeEvidence(t, t.TempDir(), map[string]string{"proof.txt": "ok\n"})
	if err := os.WriteFile(filepath.Join(work, MarkerPath), []byte("not a no-mistakes evidence branch\n"), 0o644); err != nil {
		t.Fatalf("write false marker: %v", err)
	}
	runGit(t, work, "add", MarkerPath)
	runGit(t, work, "commit", "-m", "add unrelated marker")
	runGit(t, work, "push", "origin", "HEAD:refs/heads/"+DefaultBranch)
	before := runGit(t, remote, "rev-parse", "refs/heads/"+DefaultBranch)

	if _, err := Publish(context.Background(), baseRequest(remote, work, source)); err == nil {
		t.Fatal("Publish appended to a branch with the wrong marker content")
	} else if !strings.Contains(err.Error(), "invalid "+MarkerPath+" marker") {
		t.Errorf("error %q does not explain the invalid marker", err)
	}
	if after := runGit(t, remote, "rev-parse", "refs/heads/"+DefaultBranch); after != before {
		t.Errorf("pre-existing branch moved from %s to %s", before, after)
	}
}

func TestPublish_RefusesABranchThatIsAlsoACodeBranch(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	source := writeEvidence(t, t.TempDir(), map[string]string{"proof.txt": "ok\n"})

	req := baseRequest(remote, work, source)
	req.Branch = "main"
	req.ForbiddenBranches = []string{"fm/add-login", "main"}
	if _, err := Publish(context.Background(), req); err == nil {
		t.Fatal("Publish accepted the repository default branch as the evidence branch")
	}
	if refs := runGit(t, remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Errorf("remote refs changed: %q", refs)
	}
}

func TestPublish_FailsClosedWhenTheRemoteRefusesThePush(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	source := writeEvidence(t, t.TempDir(), map[string]string{"proof.txt": "ok\n"})
	hook := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'denied: no write access' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	if _, err := Publish(context.Background(), baseRequest(remote, work, source)); err == nil {
		t.Fatal("Publish reported success although the remote refused the push")
	}
	runGitFails(t, remote, "rev-parse", "--verify", "refs/heads/"+DefaultBranch)
}

func TestPublish_FailsClosedWhenTheRemoteIsUnreadable(t *testing.T) {
	_, work := newRepoWithRemote(t)
	source := writeEvidence(t, t.TempDir(), map[string]string{"proof.txt": "ok\n"})

	req := baseRequest(filepath.Join(t.TempDir(), "missing.git"), work, source)
	if _, err := Publish(context.Background(), req); err == nil {
		t.Fatal("Publish reported success against an unreachable remote")
	}
}

func TestPublish_WorksFromADetachedShallowClone(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	// A second commit so a depth-1 clone is genuinely shallow.
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("more code\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, work, "commit", "-am", "second")
	runGit(t, work, "push", "origin", "main")

	root := t.TempDir()
	shallow := filepath.Join(root, "shallow")
	runGit(t, root, "clone", "--depth", "1", "file://"+remote, shallow)
	runGit(t, shallow, "config", "user.name", "Evidence Test")
	runGit(t, shallow, "config", "user.email", "evidence@example.com")
	runGit(t, shallow, "checkout", "--detach", "HEAD")
	if got := runGit(t, shallow, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("clone is not shallow: %q", got)
	}

	source := writeEvidence(t, t.TempDir(), map[string]string{"proof.txt": "ok\n"})
	result, err := Publish(context.Background(), baseRequest(remote, shallow, source))
	if err != nil {
		t.Fatalf("Publish from a detached shallow clone: %v", err)
	}
	if got := runGit(t, remote, "rev-parse", "refs/heads/"+DefaultBranch); got != result.CommitSHA {
		t.Errorf("evidence tip = %s, want %s", got, result.CommitSHA)
	}
	if head := runGit(t, shallow, "rev-parse", "--abbrev-ref", "HEAD"); head != "HEAD" {
		t.Errorf("publishing attached the detached HEAD to %q", head)
	}
}

func TestPublish_WithoutFilesPublishesNothing(t *testing.T) {
	remote, work := newRepoWithRemote(t)

	result, err := Publish(context.Background(), baseRequest(remote, work, t.TempDir()))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if result != nil {
		t.Errorf("empty evidence produced a publication: %+v", result)
	}
	if refs := runGit(t, remote, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Errorf("remote refs changed: %q", refs)
	}
}

func TestPublish_ReportsPublishedFilesRelativeToTheSourceDirectory(t *testing.T) {
	remote, work := newRepoWithRemote(t)
	source := writeEvidence(t, t.TempDir(), map[string]string{
		"a.txt":     "a\n",
		"sub/b.txt": "b\n",
	})

	result, err := Publish(context.Background(), baseRequest(remote, work, source))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got, want := strings.Join(result.Files, ","), "a.txt,sub/b.txt"; got != want {
		t.Errorf("files = %q, want %q", got, want)
	}
	if result.Dir != ".no-mistakes/evidence/fm/add-login" {
		t.Errorf("dir = %q", result.Dir)
	}
}
