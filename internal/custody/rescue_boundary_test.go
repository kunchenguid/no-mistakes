//go:build unix

package custody

import (
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestRescueHiddenIndexFlagsRequireRawCapture(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		for _, dirty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dirty=%v", flag, dirty), func(t *testing.T) {
				dir, _ := recoveryTestRepo(t)
				file := filepath.Join(dir, "generated")
				if err := os.WriteFile(file, []byte("base\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitRun(t, dir, "add", "generated")
				gitRun(t, dir, "commit", "-m", "generated")
				gitRun(t, dir, "update-index", flag, "generated")
				if dirty {
					if err := os.WriteFile(file, []byte("hidden working bytes\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if status := gitOutput(t, dir, "status", "--porcelain"); status != "" {
					t.Fatalf("fixture status=%q", status)
				}
				before, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
				if err != nil {
					t.Fatal(err)
				}
				p, err := PreservePartialWork(context.Background(), dir, "run", "test", "stop")
				if err != nil {
					t.Fatal(err)
				}
				if dirty {
					if p.State != "saved" {
						t.Fatalf("hidden bytes not saved: %+v", p)
					}
					if got := gitOutput(t, dir, "cat-file", "blob", p.Ref+":generated"); got != "hidden working bytes" {
						t.Errorf("rescued bytes=%q", got)
					}
					if got := gitOutput(t, dir, "cat-file", "blob", p.IndexSHA+":generated"); got != "base" {
						t.Errorf("staged bytes=%q", got)
					}
				} else if p.State != "settled" {
					t.Errorf("unchanged flagged content did not settle: %+v", p)
				}
				after, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
				if err != nil || string(after) != string(before) {
					t.Fatal("inspection modified live index flags/bytes")
				}
			})
		}
	}
}

func TestRescueAncestorLinkRefusesExternalCapture(t *testing.T) {
	for _, depth := range []string{"dir", "dir/nested"} {
		for _, consumer := range []string{"preserve", "compare"} {
			t.Run(depth+"/"+consumer, func(t *testing.T) {
				dir, _ := recoveryTestRepo(t)
				tracked := depth + "/file"
				if err := os.MkdirAll(filepath.Join(dir, depth), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, tracked), []byte("owned bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitRun(t, dir, "add", "-A")
				gitRun(t, dir, "commit", "-m", "directory")
				if err := os.WriteFile(filepath.Join(dir, "partial"), []byte("partial\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				p, err := PreservePartialWork(context.Background(), dir, "run", "test", "first")
				if err != nil || p.State != "saved" {
					t.Fatalf("saved fixture=%+v %v", p, err)
				}
				external := t.TempDir()
				sentinel := []byte("private external sentinel " + t.Name() + "\n")
				if err := os.WriteFile(filepath.Join(external, "file"), sentinel, 0o644); err != nil {
					t.Fatal(err)
				}
				blob := fmt.Sprintf("%x", sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(sentinel))), sentinel...)))
				if _, err := git.Run(context.Background(), dir, "cat-file", "-e", blob); err == nil {
					t.Fatal("sentinel already stored before capture")
				}
				if err := os.RemoveAll(filepath.Join(dir, depth)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, filepath.Join(dir, depth)); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
				if err != nil {
					t.Fatal(err)
				}
				trace := filepath.Join(t.TempDir(), "git.trace")
				t.Setenv("GIT_TRACE", trace)
				if consumer == "preserve" {
					snapshot, captureErr := PreservePartialWork(context.Background(), dir, "run", "test", "second")
					if captureErr == nil && snapshot.State != "retained" {
						t.Errorf("unsupported ancestor accepted: %+v", snapshot)
					}
				} else {
					unchanged, captureErr := PartialWorkUnchanged(context.Background(), dir, p)
					if captureErr == nil || unchanged {
						t.Errorf("comparison did not refuse ancestor: unchanged=%v err=%v", unchanged, captureErr)
					}
				}
				calls, err := os.ReadFile(trace)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(calls), "hash-object -w --no-filters -- "+filepath.Join(dir, tracked)) {
					t.Error("Git read cached descendant through ancestor link")
				}
				if _, err := git.Run(context.Background(), dir, "cat-file", "-e", blob); err == nil {
					t.Error("external sentinel copied into Git object storage")
				}
				if target, err := os.Readlink(filepath.Join(dir, depth)); err != nil || target != external {
					t.Errorf("original link lost: %q %v", target, err)
				}
				after, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
				if err != nil || string(after) != string(before) {
					t.Error("capture changed live index")
				}
				if got, err := os.ReadFile(filepath.Join(external, "file")); err != nil || string(got) != string(sentinel) {
					t.Error("external sentinel modified")
				}
			})
		}
	}
}
