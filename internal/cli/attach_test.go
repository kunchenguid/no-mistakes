package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestAttachNotInitialized verifies that running bare `no-mistakes` in a git
// repo that hasn't been initialized returns a clear error instead of panicking.
// This is the exact scenario that caused the nil pointer dereference: db.GetRepoByPath
// returns (nil, nil) for unknown repos, and the code dereferenced repo.ID without
// a nil check.
func TestAttachNotInitialized(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Start daemon so attachRun gets past EnsureDaemon + Dial.
	startTestDaemon(t, p, d)

	// Run bare `no-mistakes` — repo exists in git but is NOT initialized with no-mistakes.
	_, err = executeCmd()
	if err == nil {
		t.Fatal("bare command in uninitialized repo should return an error")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("error should mention 'not initialized', got: %v", err)
	}
}

// TestAttachNotGitRepo verifies that running bare `no-mistakes` outside any git
// repo returns a clear error.
func TestAttachNotGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	startTestDaemon(t, p, d)

	chdir(t, tmpDir)

	_, err = executeCmd()
	if err == nil {
		t.Fatal("bare command outside git repo should return an error")
	}
	if !strings.Contains(err.Error(), "not in a git repository") {
		t.Errorf("error should mention 'not in a git repository', got: %v", err)
	}
}

// TestAttachCmdUninit verifies that `no-mistakes attach` in an uninitialized
// repo returns a clear error (same underlying bug as the root command).
func TestAttachCmdUninit(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	startTestDaemon(t, p, d)

	_, err = executeCmd("attach")
	if err == nil {
		t.Fatal("attach in uninitialized repo should return an error")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("error should mention 'not initialized', got: %v", err)
	}
}

// TestAttachCmdNoGit verifies that `no-mistakes attach` outside any git repo
// returns a clear error.
func TestAttachCmdNoGit(t *testing.T) {
	tmpDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	startTestDaemon(t, p, d)

	chdir(t, tmpDir)

	_, err = executeCmd("attach")
	if err == nil {
		t.Fatal("attach outside git repo should return an error")
	}
	if !strings.Contains(err.Error(), "not in a git repository") {
		t.Errorf("error should mention 'not in a git repository', got: %v", err)
	}
}
