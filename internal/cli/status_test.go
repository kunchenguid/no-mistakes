package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestStatusAlwaysRendersCachedLocalState(t *testing.T) {
	setupTestRepo(t)
	if _, err := executeCmd("init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	clean, err := executeCmd("status")
	if err != nil {
		t.Fatalf("clean status: %v", err)
	}
	for _, want := range []string{"repo:", "daemon:", "local state:  cached:", "(clean;", "no active run"} {
		if !strings.Contains(clean, want) {
			t.Fatalf("clean status missing %q:\n%s", want, clean)
		}
	}

	if err := os.WriteFile("uncommitted.txt", []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("make worktree dirty: %v", err)
	}

	dirty, err := executeCmd("status")
	if err != nil {
		t.Fatalf("dirty status: %v", err)
	}
	for _, want := range []string{"repo:", "daemon:", "local state:  cached:", "(dirty:", "no active run"} {
		if !strings.Contains(dirty, want) {
			t.Fatalf("dirty status missing %q:\n%s", want, dirty)
		}
	}
}

func TestCachedBranchSummary(t *testing.T) {
	tests := []struct {
		name  string
		state branchsync.State
		want  string
	}{
		{
			name: "clean branch",
			state: branchsync.State{
				State: branchsync.StateSynchronized,
				Local: branchsync.LocalState{Branch: "feature/state", Head: "0123456789abcdef", Clean: true},
			},
			want: "cached: feature/state 01234567 (clean; already synchronized with the pipeline-pushed head)",
		},
		{
			name: "dirty branch",
			state: branchsync.State{
				State: branchsync.StateDirty,
				Local: branchsync.LocalState{Branch: "feature/state", Head: "fedcba9876543210", Reason: "uncommitted changes"},
			},
			want: "cached: feature/state fedcba98 (dirty: uncommitted changes; dirty)",
		},
		{
			name:  "unavailable",
			state: branchsync.State{State: branchsync.StateAmbiguousContext},
			want:  "cached: unavailable (ambiguous context)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cachedBranchSummary(tt.state); got != tt.want {
				t.Fatalf("cachedBranchSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatusFingerprintIncludesCachedSummary(t *testing.T) {
	run := &db.Run{ID: "run-1", Branch: "feature/test", Status: "running", HeadSHA: "head-one"}
	before := statusFingerprint("repo", "running", run, "cached: main 01234567 (clean; synchronized)")
	after := statusFingerprint("repo", "running", run, "cached: main 89abcdef (dirty; dirty)")
	if before == after {
		t.Fatal("changing displayed cached evidence must change the status fingerprint")
	}
}
