package custody

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// The oracle in these tests is the commit subject: every commit is created
// with a distinct subject and the reword appends a distinct suffix, so the
// subjects listed by `git log from..to` name exactly which commits the
// remapped range marks as fixer-written, independently of the ~n arithmetic.

func TestRemapRewordedRange_RangeStartingInsideTheRewordedHistory(t *testing.T) {
	repo, rebased, recorded := rewordTestHistory(t, 4)
	// Only c3 and c4 are fixer-written; c1 and c2 are earlier history above
	// the post-rebase head.
	c := chain(t, repo, rebased, recorded)
	rewritten := rewordChain(t, repo, rebased, c, map[int]bool{0: true, 1: true, 2: true, 3: true}, "v2")
	d, repoID := rewordTestDB(t)
	upsertRange(t, d, repoID, c[1], c[3])

	remap(t, d, repo, repoID, rebased, recorded, rewritten)

	assertRangeSubjects(t, d, repo, repoID, []string{"c3 v2", "c4 v2"})
}

func TestRemapRewordedRange_PartialReword(t *testing.T) {
	repo, rebased, recorded := rewordTestHistory(t, 4)
	c := chain(t, repo, rebased, recorded)
	// Only c3 and c4 are reworded, so c1 and c2 stay shared by both heads.
	rewritten := rewordChain(t, repo, rebased, c, map[int]bool{2: true, 3: true}, "v2")
	d, repoID := rewordTestDB(t)

	t.Run("range starting below the reworded commits", func(t *testing.T) {
		upsertRange(t, d, repoID, c[0], c[3])
		remap(t, d, repo, repoID, rebased, recorded, rewritten)
		assertRangeSubjects(t, d, repo, repoID, []string{"c2", "c3 v2", "c4 v2"})
	})
	t.Run("range starting at the first reworded commit", func(t *testing.T) {
		upsertRange(t, d, repoID, c[2], c[3])
		remap(t, d, repo, repoID, rebased, recorded, rewritten)
		assertRangeSubjects(t, d, repo, repoID, []string{"c4 v2"})
	})
	t.Run("range ending before the reworded commits", func(t *testing.T) {
		upsertRange(t, d, repoID, c[0], c[1])
		remap(t, d, repo, repoID, rebased, recorded, rewritten)
		assertRangeSubjects(t, d, repo, repoID, []string{"c2"})
	})
}

func TestRemapRewordedRange_RepeatedRewords(t *testing.T) {
	repo, rebased, recorded := rewordTestHistory(t, 4)
	c := chain(t, repo, rebased, recorded)
	d, repoID := rewordTestDB(t)
	upsertRange(t, d, repoID, c[1], c[3])

	first := rewordChain(t, repo, rebased, c, map[int]bool{1: true, 2: true, 3: true}, "v2")
	remap(t, d, repo, repoID, rebased, recorded, first)
	assertRangeSubjects(t, d, repo, repoID, []string{"c3 v2", "c4 v2"})

	second := rewordChain(t, repo, rebased, chain(t, repo, rebased, first), map[int]bool{3: true}, "v3")
	remap(t, d, repo, repoID, rebased, first, second)
	assertRangeSubjects(t, d, repo, repoID, []string{"c3 v2", "c4 v2 v3"})
}

func TestRemapRewordedRange_NoRangeMarksTheRewordedSpan(t *testing.T) {
	repo, rebased, recorded := rewordTestHistory(t, 3)
	c := chain(t, repo, rebased, recorded)
	rewritten := rewordChain(t, repo, rebased, c, map[int]bool{0: true, 1: true, 2: true}, "v2")
	d, repoID := rewordTestDB(t)

	remap(t, d, repo, repoID, rebased, recorded, rewritten)

	assertRangeSubjects(t, d, repo, repoID, []string{"c1 v2", "c2 v2", "c3 v2"})
}

// rewordTestHistory creates base -> rebased -> c1..cN, each with its own tree.
func rewordTestHistory(t *testing.T, n int) (repo, rebased, head string) {
	t.Helper()
	repo, _ = recoveryTestRepo(t)
	gitRun(t, repo, "commit", "--allow-empty", "-m", "rebased")
	rebased = gitOutput(t, repo, "rev-parse", "HEAD")
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("c%d", i)
		if err := os.WriteFile(filepath.Join(repo, name+".txt"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, repo, "add", name+".txt")
		gitRun(t, repo, "commit", "-m", name)
	}
	return repo, rebased, gitOutput(t, repo, "rev-parse", "HEAD")
}

func chain(t *testing.T, repo, rebased, head string) []string {
	t.Helper()
	return strings.Fields(gitOutput(t, repo, "rev-list", "--reverse", rebased+".."+head))
}

// rewordChain rebuilds commits on top of rebased with unchanged trees,
// appending suffix to the subject of each commit whose index is in reword.
func rewordChain(t *testing.T, repo, rebased string, commits []string, reword map[int]bool, suffix string) string {
	t.Helper()
	parent := rebased
	rewritten := false
	for i, commit := range commits {
		if !reword[i] && !rewritten {
			parent = commit
			continue
		}
		rewritten = true
		subject := gitOutput(t, repo, "log", "-1", "--format=%s", commit)
		if reword[i] {
			subject += " " + suffix
		}
		parent = gitOutput(t, repo, "commit-tree", commit+"^{tree}", "-p", parent, "-m", subject)
	}
	if err := VerifyReword(context.Background(), repo, rebased, commits[len(commits)-1], parent); err != nil {
		t.Fatalf("test reword is not a metadata-only rewrite: %v", err)
	}
	return parent
}

func rewordTestDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	r, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	return d, r.ID
}

func upsertRange(t *testing.T, d *db.DB, repoID, from, to string) {
	t.Helper()
	if err := d.UpsertUncertifiedPipelineRange(repoID, "feature", from, to, "source-run"); err != nil {
		t.Fatal(err)
	}
}

func remap(t *testing.T, d *db.DB, repo, repoID, rebased, recorded, rewritten string) {
	t.Helper()
	if err := RemapRewordedRange(context.Background(), d, repo, repoID, "feature", "run-1", rebased, recorded, rewritten); err != nil {
		t.Fatalf("remap: %v", err)
	}
}

func assertRangeSubjects(t *testing.T, d *db.DB, repo, repoID string, want []string) {
	t.Helper()
	rng, err := d.GetUncertifiedPipelineRange(repoID, "feature")
	if err != nil || rng == nil {
		t.Fatalf("range = %v, %v", rng, err)
	}
	cmd := exec.Command("git", "log", "--reverse", "--format=%s", rng.FromSHA+".."+rng.ToSHA)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log %s..%s: %v", rng.FromSHA, rng.ToSHA, err)
	}
	if got := strings.Split(strings.TrimSpace(string(out)), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("remapped range commits = %q, want %q", got, want)
	}
}
