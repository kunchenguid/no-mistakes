package fsutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveAllHonorsCancelledContexts(t *testing.T) {
	for _, cause := range []string{"cancel", "deadline"} {
		t.Run(cause, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "retained")
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(path, "unique.txt")
			if err := os.WriteFile(file, []byte("retained bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if cause == "deadline" {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			}
			cancel()
			if err := RemoveAll(ctx, path); !errors.Is(err, want) {
				t.Fatalf("cancelled removal = %v, want %v", err, want)
			}
			if got, err := os.ReadFile(file); err != nil || string(got) != "retained bytes" {
				t.Fatalf("cancelled removal changed bytes: %q, %v", got, err)
			}
		})
	}
}

func TestRemoveAllRemovesTreesAndMissingPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(path, "nested", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"top.txt", filepath.Join("nested", "file.txt")} {
		if err := os.WriteFile(filepath.Join(path, name), []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("batch-%03d", i)), []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := RemoveAll(context.Background(), path); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("tree survived removal: %v", err)
		}
	}
}

func TestRemoveAllIfSameRequiresOwnedIdentity(t *testing.T) {
	path := t.TempDir()
	file := filepath.Join(path, "unique.txt")
	if err := os.WriteFile(file, []byte("retained bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	owned, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []os.FileInfo{nil, unrelated} {
		if err := RemoveAllIfSame(context.Background(), path, expected); !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("unproven removal = %v, want refusal", err)
		}
		if got, err := os.ReadFile(file); err != nil || string(got) != "retained bytes" {
			t.Fatalf("unproven removal changed bytes: %q, %v", got, err)
		}
	}
	if err := RemoveAllIfSame(context.Background(), path, owned); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned removal did not remove the tree: %v", err)
	}
}

func TestRemoveAllDoesNotFollowSymlinks(t *testing.T) {
	for _, topLevel := range []bool{true, false} {
		name := "nested"
		if topLevel {
			name = "top-level"
		}
		t.Run(name, func(t *testing.T) {
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "unique.txt")
			if err := os.WriteFile(sentinel, []byte("outside bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "target")
			link := path
			if !topLevel {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(path, "link")
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			if err := RemoveAll(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("selected entry survived removal: %v", err)
			}
			if got, err := os.ReadFile(sentinel); err != nil || string(got) != "outside bytes" {
				t.Fatalf("removal followed a symlink: %q, %v", got, err)
			}
		})
	}
}
