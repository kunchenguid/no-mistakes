package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestTestEvidenceDir_AlwaysOutsideTheWorktree(t *testing.T) {
	got := testEvidenceDir("run-123")
	want := filepath.Join(os.TempDir(), "no-mistakes-evidence", "run-123")
	if got != want {
		t.Errorf("evidence dir = %q, want %q", got, want)
	}
}

func TestEvidenceBranchSlug_KeepsBranchStructureAndDropsUnsafeSegments(t *testing.T) {
	cases := map[string]string{
		"feature/add-login":  "feature/add-login",
		"../../etc/pa ss~wd": "etc/pa-ss-wd",
		"///":                "",
	}
	for branch, want := range cases {
		got := strings.Join(evidenceBranchSlug(branch), "/")
		if got != want {
			t.Errorf("slug(%q) = %q, want %q", branch, got, want)
		}
	}
}

func TestReusableBrowserContext_ReusesCompatibleEvidenceAcrossReruns(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	first := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	second := *first
	secondRun := *first.Run
	secondRun.ID = "rerun-2"
	second.Run = &secondRun

	firstEvidence, err := reusableEvidenceDir(first)
	if err != nil {
		t.Fatal(err)
	}
	secondEvidence, err := reusableEvidenceDir(&second)
	if err != nil {
		t.Fatal(err)
	}
	if firstEvidence != secondEvidence {
		t.Fatalf("compatible evidence dirs differ across reruns: %q != %q", firstEvidence, secondEvidence)
	}
	if taskBrowserRuntimeDir(first) != taskBrowserRuntimeDir(&second) {
		t.Fatal("task-local browser runtime configuration was not reused")
	}

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("different tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changedEvidence, err := reusableEvidenceDir(&second)
	if err != nil {
		t.Fatal(err)
	}
	if changedEvidence == firstEvidence {
		t.Fatal("incompatible worktree state reused stale browser evidence")
	}
	if taskBrowserRuntimeDir(first) != taskBrowserRuntimeDir(&second) {
		t.Fatal("runtime configuration should remain task-local across tree changes")
	}
}
