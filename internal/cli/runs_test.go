package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPrintRunLineShowsRunID: `no-mistakes runs` must surface the run ID so a
// parallel run (e.g. an older parked one) can be targeted with
// `axi status --run <id>` without querying the database by hand.
func TestPrintRunLineShowsRunID(t *testing.T) {
	run := &db.Run{
		ID:        "01HZEXAMPLERUNID000000000",
		Branch:    "feature/parked",
		HeadSHA:   "abcdef1234567890",
		Status:    types.RunRunning,
		CreatedAt: 1700000000,
	}

	var out bytes.Buffer
	printRunLine(&out, run)

	got := out.String()
	if !strings.Contains(got, run.ID) {
		t.Fatalf("runs line missing run ID %q, got:\n%s", run.ID, got)
	}
	if !strings.Contains(got, run.Branch) {
		t.Fatalf("runs line missing branch %q, got:\n%s", run.Branch, got)
	}
}
