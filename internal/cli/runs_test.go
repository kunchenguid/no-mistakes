package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestRunsLimit(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatalf("gate.Init failed: %v", err)
	}

	// Insert many runs.
	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 15; i++ {
		if _, err := d.InsertRun(repo.ID, "branch", "sha", "base"); err != nil {
			t.Fatal(err)
		}
	}

	// Default limit should show max 10 runs.
	out, err := executeCmd("runs")
	if err != nil {
		t.Fatalf("runs failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// Count data lines (skip header and empty lines).
	dataLines := 0
	for _, line := range lines {
		if strings.Contains(line, "branch") && strings.Contains(line, "pending") {
			dataLines++
		}
	}
	if dataLines > 10 {
		t.Errorf("default runs output should show at most 10 runs, got %d", dataLines)
	}
}
