package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiff(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	// get initial commit SHA
	base := run(t, dir, "git", "rev-parse", "HEAD")

	// make a change and commit
	writeFile(t, filepath.Join(dir, "file.txt"), "hello\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "add file")
	head := run(t, dir, "git", "rev-parse", "HEAD")

	diff, err := Diff(ctx, dir, base, head)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if !strings.Contains(diff, "file.txt") {
		t.Fatalf("diff should mention file.txt, got: %q", diff)
	}
	if !strings.Contains(diff, "+hello") {
		t.Fatalf("diff should contain +hello, got: %q", diff)
	}
}

func TestDiffEmpty(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()
	head := run(t, dir, "git", "rev-parse", "HEAD")

	diff, err := Diff(ctx, dir, head, head)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if diff != "" {
		t.Fatalf("expected empty diff, got: %q", diff)
	}
}

func TestDiffAgainstEmptyTree(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()
	head := run(t, dir, "git", "rev-parse", "HEAD")
	const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

	diff, err := Diff(ctx, dir, emptyTreeSHA, head)
	if err != nil {
		t.Fatalf("Diff against empty tree failed: %v", err)
	}
	if !strings.Contains(diff, "README.md") {
		t.Fatalf("diff should mention README.md, got: %q", diff)
	}
	if !strings.Contains(diff, "+# test") {
		t.Fatalf("diff should include initial file contents, got: %q", diff)
	}
}

func TestDiffHead(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	// No changes — should be empty
	diff, err := DiffHead(ctx, dir)
	if err != nil {
		t.Fatalf("DiffHead (clean) failed: %v", err)
	}
	if diff != "" {
		t.Fatalf("expected empty diff for clean tree, got: %q", diff)
	}

	// Unstaged changes
	writeFile(t, filepath.Join(dir, "new.txt"), "content\n")
	run(t, dir, "git", "add", "new.txt")
	writeFile(t, filepath.Join(dir, "new.txt"), "modified\n")

	diff, err = DiffHead(ctx, dir)
	if err != nil {
		t.Fatalf("DiffHead (unstaged) failed: %v", err)
	}
	if !strings.Contains(diff, "new.txt") {
		t.Fatalf("diff should mention new.txt, got: %q", diff)
	}
	if !strings.Contains(diff, "+modified") {
		t.Fatalf("diff should contain +modified, got: %q", diff)
	}
}

func TestDiffHead_StagedChanges(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	// Staged but uncommitted changes
	writeFile(t, filepath.Join(dir, "staged.txt"), "staged content\n")
	run(t, dir, "git", "add", "staged.txt")

	diff, err := DiffHead(ctx, dir)
	if err != nil {
		t.Fatalf("DiffHead (staged) failed: %v", err)
	}
	if !strings.Contains(diff, "staged.txt") {
		t.Fatalf("diff should mention staged.txt, got: %q", diff)
	}
	if !strings.Contains(diff, "+staged content") {
		t.Fatalf("diff should contain +staged content, got: %q", diff)
	}
}

func TestLog(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	base := run(t, dir, "git", "rev-parse", "HEAD")

	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "first change")

	writeFile(t, filepath.Join(dir, "b.txt"), "b\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "second change")

	head := run(t, dir, "git", "rev-parse", "HEAD")

	log, err := Log(ctx, dir, base, head)
	if err != nil {
		t.Fatalf("Log failed: %v", err)
	}
	if !strings.Contains(log, "first change") {
		t.Fatalf("log should contain first change, got: %q", log)
	}
	if !strings.Contains(log, "second change") {
		t.Fatalf("log should contain second change, got: %q", log)
	}
}

func TestLogAgainstEmptyTree(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()
	head := run(t, dir, "git", "rev-parse", "HEAD")
	const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

	log, err := Log(ctx, dir, emptyTreeSHA, head)
	if err != nil {
		t.Fatalf("Log against empty tree failed: %v", err)
	}
	if !strings.Contains(log, "initial") {
		t.Fatalf("log should contain initial commit, got: %q", log)
	}
}

func TestHeadSHA(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	sha, err := HeadSHA(ctx, dir)
	if err != nil {
		t.Fatalf("HeadSHA failed: %v", err)
	}
	if len(sha) != 40 {
		t.Fatalf("expected 40-char SHA, got %d chars: %q", len(sha), sha)
	}
}
