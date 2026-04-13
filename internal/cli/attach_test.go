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
func TestAttachNotInitializedCommands(t *testing.T) {
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

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "root", args: nil},
		{name: "attach", args: []string{"attach"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err = executeCmd(test.args...)
			if err == nil {
				t.Fatalf("%s command in uninitialized repo should return an error", test.name)
			}
			if !strings.Contains(err.Error(), "not initialized") {
				t.Errorf("error should mention 'not initialized', got: %v", err)
			}
		})
	}
}

// TestAttachNotGitRepo verifies that running bare `no-mistakes` outside any git
// repo returns a clear error.
func TestAttachNotGitRepoCommands(t *testing.T) {
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

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "root", args: nil},
		{name: "attach", args: []string{"attach"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err = executeCmd(test.args...)
			if err == nil {
				t.Fatalf("%s command outside git repo should return an error", test.name)
			}
			if !strings.Contains(err.Error(), "not in a git repository") {
				t.Errorf("error should mention 'not in a git repository', got: %v", err)
			}
		})
	}
}
