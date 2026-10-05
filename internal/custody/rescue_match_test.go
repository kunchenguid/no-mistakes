//go:build unix

package custody

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRescueSavedComparisonUsesExactRawIndexAndWorkingContent(t *testing.T) {
	for _, change := range []string{"none", "working", "staged", "mode", "symlink", "deletion", "untracked", "ignored", "head", "filter"} {
		t.Run(change, func(t *testing.T) {
			dir, _ := recoveryTestRepo(t)
			write := func(name string, bytes []byte) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), bytes, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("edit", []byte("base\n"))
			write("gone", []byte("keep\n"))
			gitRun(t, dir, "add", "-A")
			gitRun(t, dir, "commit", "-m", "base")
			write("edit", []byte("staged\n"))
			gitRun(t, dir, "add", "edit")
			write("edit", []byte{0, 255, 'W'})
			write("new", []byte("new bytes\n"))
			if err := os.Symlink("edit", filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			p, err := PreservePartialWork(context.Background(), dir, "run", "test", "stop")
			if err != nil || p.State != "saved" {
				t.Fatalf("initial capture=%+v %v", p, err)
			}
			index := gitOutput(t, dir, "ls-files", "--stage")
			refs := gitOutput(t, dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/no-mistakes/rescue/")
			switch change {
			case "working":
				write("edit", []byte("different working\n"))
			case "staged":
				write("edit", []byte("different staged\n"))
				gitRun(t, dir, "add", "edit")
				write("edit", []byte{0, 255, 'W'})
			case "mode":
				if err := os.Chmod(filepath.Join(dir, "new"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(filepath.Join(dir, "link")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("gone", filepath.Join(dir, "link")); err != nil {
					t.Fatal(err)
				}
			case "deletion":
				if err := os.Remove(filepath.Join(dir, "gone")); err != nil {
					t.Fatal(err)
				}
			case "untracked":
				write("more", []byte("extra\n"))
			case "ignored":
				write(".gitignore", []byte("private\n"))
				write("private", []byte("ignored bytes\n"))
			case "head":
				gitRun(t, dir, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "different parent")
			case "filter":
				gitRun(t, dir, "config", "filter.raw.clean", "echo unsafe > filter-ran")
				gitRun(t, dir, "config", "core.attributesfile", filepath.Join(t.TempDir(), "attributes"))
				attributes := gitOutput(t, dir, "config", "core.attributesfile")
				if err := os.WriteFile(attributes, []byte("edit filter=raw\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := gitOutput(t, dir, "ls-files", "--stage")
			unchanged, err := PartialWorkUnchanged(context.Background(), dir, p)
			want := change == "none" || change == "filter"
			if err != nil || unchanged != want {
				t.Errorf("raw comparison=%v %v want=%v", unchanged, err, want)
			}
			if got := gitOutput(t, dir, "ls-files", "--stage"); got != before {
				t.Error("comparison changed live index")
			}
			if got := gitOutput(t, dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/no-mistakes/rescue/"); got != refs {
				t.Error("comparison created/replaced rescue ref")
			}
			if change == "none" && before != index {
				t.Error("unchanged fixture index drifted")
			}
			if _, err := os.Stat(filepath.Join(dir, "filter-ran")); !os.IsNotExist(err) {
				t.Error("comparison invoked clean filter")
			}
		})
	}
}
