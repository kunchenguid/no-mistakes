package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/procreap"
)

func TestRecursiveCleanupStopsDuringDeletion(t *testing.T) {
	for _, operation := range []string{"worktree-fallback", "evidence", "run-logs"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "leftover")
			payload := filepath.Join(path, "large", "subtree")
			if err := os.MkdirAll(payload, 0o755); err != nil {
				t.Fatal(err)
			}
			const files = 4096
			for i := 0; i < files; i++ {
				if err := os.WriteFile(filepath.Join(payload, fmt.Sprintf("file-%04d", i)), []byte("owned bytes"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			previous := sweepRunWorktrees
			sweepRunWorktrees = func(context.Context, string, []procreap.Worktree, string) {}
			t.Cleanup(func() { sweepRunWorktrees = previous })
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			removed := false
			go func() {
				defer close(done)
				switch operation {
				case "worktree-fallback":
					wt := orphanWorktree{gateDir: filepath.Join(root, "missing.git"), dir: path, repoID: "fixture", runID: "fixture-run"}
					removed = removeOrphanWorktrees(ctx, root, []orphanWorktree{wt}, "test cancellation") != 0
				case "evidence":
					removed = removeEvidenceDir(ctx, path, "fixture-run")
				case "run-logs":
					removed = removeRunLogDir(ctx, path, "fixture-run")
				}
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("fixture deletion did not stop")
				}
			})
			deadline := time.Now().Add(5 * time.Second)
			for {
				entries, err := os.ReadDir(payload)
				if err != nil {
					t.Fatalf("deletion finished before cancellation could be observed: %v", err)
				}
				if len(entries) < files {
					cancel()
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture deletion did not start")
				}
				select {
				case <-done:
					t.Fatal("fixture deletion ended before making progress")
				case <-time.After(time.Millisecond):
				}
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("recursive deletion ignored cancellation")
			}
			entries, err := os.ReadDir(payload)
			if removed || err != nil || len(entries) == 0 || len(entries) >= files {
				t.Fatalf("unfinished directory was not retained: removed=%t remaining=%d error=%v", removed, len(entries), err)
			}
			if got, err := os.ReadFile(filepath.Join(payload, entries[0].Name())); err != nil || string(got) != "owned bytes" {
				t.Fatalf("unfinished cleanup changed retained bytes: %q, %v", got, err)
			}
		})
	}
}
