package custody

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestFixProgressRescuePreservesIndexAndWorkingBytes(t *testing.T) {
	dir, _ := recoveryTestRepo(t)
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("edit", []byte("base\n"))
	write("gone", []byte("gone\n"))
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "files")
	head := gitOutput(t, dir, "rev-parse", "HEAD")
	write("edit", []byte("staged\n"))
	gitRun(t, dir, "add", "edit")
	write("edit", []byte("unfinished\n"))
	write("new.bin", []byte{0, 255, 13, 10})
	if err := os.Remove(filepath.Join(dir, "gone")); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := PreservePartialWork(context.Background(), dir, "run-1", "review", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != "saved" || snapshot.ParentHead != head {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	for spec, expected := range map[string]string{snapshot.Ref + ":edit": "unfinished\n", snapshot.Ref + "^2:edit": "staged\n", snapshot.Ref + ":new.bin": string([]byte{0, 255, 13, 10})} {
		got, err := git.RunRaw(context.Background(), dir, "cat-file", "blob", spec)
		if err != nil || string(got) != expected {
			t.Fatalf("%s = %q, %v", spec, got, err)
		}
	}
	if _, err := git.Run(context.Background(), dir, "cat-file", "-e", snapshot.Ref+":gone"); err == nil {
		t.Fatal("deleted file saved as present")
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || string(after) != string(before) {
		t.Fatal("capture modified live index")
	}
	if got := gitOutput(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatal("capture moved HEAD")
	}
	if err := ValidatePartialWork(context.Background(), dir, snapshot); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "update-ref", snapshot.Ref, head)
	if err := ValidatePartialWork(context.Background(), dir, snapshot); err == nil {
		t.Fatal("moved ref accepted")
	}
}

func TestRescueCleanSuperprojectRetainsIgnoredGitlinkBytes(t *testing.T) {
	dir, _ := recoveryTestRepo(t)
	sub, _ := recoveryTestRepo(t)
	if err := os.WriteFile(filepath.Join(sub, ".gitignore"), []byte("private\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sub, "add", ".gitignore")
	gitRun(t, sub, "commit", "-m", "ignore private bytes")
	gitRun(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", sub, "module")
	gitRun(t, dir, "commit", "-m", "initialized submodule")
	file := filepath.Join(dir, "module", "private")
	if err := os.WriteFile(file, []byte("nested unfinished bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := gitOutput(t, dir, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatalf("not a clean superproject: %q", status)
	}
	snapshot, err := PreservePartialWork(context.Background(), dir, "run-1", "review", "stop-1")
	if err != nil || snapshot.State != "retained" || snapshot.Path != dir {
		t.Fatalf("nested bytes not retained: %+v %v", snapshot, err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "nested unfinished bytes\n" {
		t.Fatalf("nested bytes changed: %q %v", got, err)
	}
}

func TestFixProgressRescueIgnoredWorkRetainsOriginal(t *testing.T) {
	dir, _ := recoveryTestRepo(t)
	for name, data := range map[string]string{".gitignore": "private\n", "private": "useful unfinished bytes\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := PreservePartialWork(context.Background(), dir, "run-1", "test", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != "retained" || snapshot.Reason == "" || snapshot.Ref == "" {
		t.Fatalf("ignored bytes incorrectly declared saved: %+v", snapshot)
	}
}

func TestFixProgressRescueCaptureIdempotentAcrossCleanup(t *testing.T) {
	dir, _ := recoveryTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "unfinished"), []byte("C"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_AUTHOR_DATE", "2000-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2000-01-01T00:00:00Z")
	first, err := PreservePartialWork(context.Background(), dir, "run-1", "review", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_AUTHOR_DATE", "2001-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2001-01-01T00:00:00Z")
	second, err := PreservePartialWork(context.Background(), dir, "run-1", "review", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA != second.SHA {
		t.Fatal("cleanup manufactured new evidence for unchanged partial bytes")
	}
}

func TestFixProgressRescueInspectionDoesNotExecuteCleanFilter(t *testing.T) {
	dir, _ := recoveryTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "filtered.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "filtered.txt")
	gitRun(t, dir, "commit", "-m", "file")
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("filtered.txt filter=emergency\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "config", "filter.emergency.clean", "git update-ref refs/no-mistakes/filter-invoked HEAD")
	if err := os.WriteFile(filepath.Join(dir, "filtered.txt"), []byte("part"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := PreservePartialWork(context.Background(), dir, "run-1", "review", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := git.RunRaw(context.Background(), dir, "cat-file", "blob", p.Ref+":filtered.txt")
	if err != nil || string(got) != "part" {
		t.Fatalf("filter transformed rescue bytes: %q %v", got, err)
	}
	if _, err := git.Run(context.Background(), dir, "rev-parse", "--verify", "refs/no-mistakes/filter-invoked"); err == nil {
		t.Fatal("emergency inspection executed repository clean filter")
	}
}

func TestFixProgressRescuePreservesGitModesOnLimitedFilesystems(t *testing.T) {
	dir, _ := recoveryTestRepo(t)
	gitRun(t, dir, "config", "core.filemode", "false")
	gitRun(t, dir, "config", "core.symlinks", "false")
	for name, content := range map[string]string{"executable": "base", "link": "target"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", "executable")
	gitRun(t, dir, "update-index", "--chmod=+x", "executable")
	blob := gitOutput(t, dir, "hash-object", "-w", "--no-filters", "link")
	gitRun(t, dir, "update-index", "--add", "--cacheinfo", "120000", blob, "link")
	gitRun(t, dir, "commit", "-m", "modes")
	if err := os.WriteFile(filepath.Join(dir, "executable"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := PreservePartialWork(context.Background(), dir, "run-1", "review", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]string{"executable": "100755", "link": "120000"} {
		got := gitOutput(t, dir, "ls-tree", p.Ref, "--", name)
		if !strings.HasPrefix(got, mode+" ") {
			t.Fatalf("%s Git mode lost: %s", name, got)
		}
	}
}
