package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestAxiStatusCountsCommitsAfterTheReviewApprovedHead reads the count from the
// gate repository the run's worktree shares objects with, and omits it once
// the head is the approved head again.
func TestAxiStatusCountsCommitsAfterTheReviewApprovedHead(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(file string) string {
		if err := os.WriteFile(filepath.Join(work, file), []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
		git(work, "add", file)
		git(work, "commit", "-m", file)
		return git(work, "rev-parse", "HEAD")
	}
	git(work, "init", "-b", "feature")
	approved := commit("feature.txt")
	commit("README.md")
	head := commit("lint.txt")
	gateDir := p.RepoDir(repo.ID)
	if err := os.MkdirAll(filepath.Dir(gateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	git(filepath.Dir(gateDir), "clone", "--bare", work, filepath.Base(gateDir))

	run, err := d.InsertRun(repo.ID, "feature", head, approved)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, approved); err != nil {
		t.Fatal(err)
	}
	env := &axiEnv{p: p, d: d, repo: repo}
	render := func() string {
		rv := runViewFromDB(run, nil, d)
		annotateRunView(env, &rv)
		return axiDoc(runObjectField(rv))
	}
	if out := render(); !strings.Contains(out, "  post_review_commits: 2\n") {
		t.Fatalf("status does not count the two commits after the approved head:\n%s", out)
	}

	if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	if out := render(); strings.Contains(out, "post_review_commits") {
		t.Fatalf("status reports commits after review on a head Review approved:\n%s", out)
	}
}
