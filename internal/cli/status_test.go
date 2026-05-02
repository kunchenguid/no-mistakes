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

func TestStatusWithShortHeadSHA(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatal(err)
	}

	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	r, err := d.InsertRun(repo.ID, "feature/short-sha", "abc123", "0000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(r.ID, "running"); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("status")
	if err != nil {
		t.Fatalf("status failed: %v\noutput: %s", err, out)
	}

	if !strings.Contains(out, "abc123") {
		t.Errorf("expected full short head SHA 'abc123', got: %s", out)
	}
	if strings.Contains(out, "00000000") {
		t.Errorf("status output should show the active run head SHA, got: %s", out)
	}
}
