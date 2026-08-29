package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestWedgedCustodyRecordSettlesThroughTheCLI walks the whole issue #824
// operator journey on the surface the operator actually types, rather than on
// the branchsync service the unit tests drive directly: a terminal run whose
// recorded pipeline head is in no object store leaves the branch
// pipeline_owned, and before this change every command the surfaces offered
// refused on that same unverifiable head, so the branch could never be taken
// back.
//
// The journey has to hold end to end, not just report a next action: the
// advertised command must be the one that completes, it must leave the gate
// branch at the head the operator kept, and a second read must show the branch
// released - which is the only proof the dead end is actually gone.
func TestWedgedCustodyRecordSettlesThroughTheCLI(t *testing.T) {
	runID, p, local := wedgedCustodyAbortFixture(t)
	gateDir, keptHead := wedgedFixtureGateAndHead(t, p, runID, local)
	displacedGateHead := cliGit(t, gateDir, "rev-parse", "refs/heads/feature/wedged^{commit}")
	if displacedGateHead == keptHead {
		t.Fatalf("fixture must start with the gate branch off the kept head: %s", keptHead)
	}

	blocked, err := executeCmd("axi", "sync", "--check")
	t.Logf("STEP 1 - `no-mistakes axi sync --check` on the wedged record:\n%s", blocked)
	if err == nil {
		t.Fatalf("a wedged custody record must refuse the ordinary plan:\n%s", blocked)
	}
	for _, want := range []string{
		"state: pipeline_owned",
		"code: return_custody_keep_local",
		"command: no-mistakes axi sync --recover --keep-local",
		"Run `no-mistakes axi sync --recover --keep-local`",
	} {
		if !strings.Contains(blocked, want) {
			t.Errorf("wedged plan missing %q:\n%s", want, blocked)
		}
	}

	// The plain recovery is the command that can only refuse on this record,
	// so the surface must not be quietly settling it under --recover alone.
	refused, err := executeCmd("axi", "sync", "--recover")
	t.Logf("STEP 2 - `no-mistakes axi sync --recover` (no --keep-local) still refuses:\n%s", refused)
	if err == nil {
		t.Fatalf("the plain recovery must refuse a record with no verifiable head:\n%s", refused)
	}
	if strings.Contains(refused, "state: custody_returned") {
		t.Errorf("the plain recovery settled a wedged record:\n%s", refused)
	}
	if cliGit(t, gateDir, "rev-parse", "refs/heads/feature/wedged^{commit}") != displacedGateHead {
		t.Error("a refused recovery moved the gate branch")
	}

	settled, err := executeCmd("axi", "sync", "--recover", "--keep-local")
	t.Logf("STEP 3 - the advertised `no-mistakes axi sync --recover --keep-local` completes:\n%s", settled)
	if err != nil {
		t.Fatalf("the advertised settlement must complete: %v\n%s", err, settled)
	}
	for _, want := range []string{"state: custody_returned", "recovered: true", "safety: custody_returned"} {
		if !strings.Contains(settled, want) {
			t.Errorf("settlement output missing %q:\n%s", want, settled)
		}
	}

	// The worktree is never touched, and the gate branch follows the head the
	// operator kept - both are the settlement's stated contract.
	if head := cliGit(t, local, "rev-parse", "HEAD"); head != keptHead {
		t.Errorf("settlement moved the worktree HEAD: %s -> %s", keptHead, head)
	}
	if gateHead := cliGit(t, gateDir, "rev-parse", "refs/heads/feature/wedged^{commit}"); gateHead != keptHead {
		t.Errorf("gate branch = %s, want the kept local head %s", gateHead, keptHead)
	}
	// The displaced gate head is never dropped on the floor: it stays
	// reachable through the run's gate anchor.
	if anchored := cliGit(t, gateDir, "rev-parse", custody.RecoveryGateRef(runID)+"^{commit}"); anchored != displacedGateHead {
		t.Errorf("displaced gate head anchor = %s, want %s", anchored, displacedGateHead)
	}

	after, err := executeCmd("axi", "sync", "--check")
	t.Logf("STEP 4 - `no-mistakes axi sync --check` after settling:\n%s", after)
	if strings.Contains(after, "state: pipeline_owned") {
		t.Errorf("the branch is still held by the terminal run after settling:\n%s", after)
	}

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CustodyReturnedAt == nil {
		t.Error("the custody return was not recorded on the run row")
	}
}

// wedgedFixtureGateAndHead resolves the fixture's real gate directory and the
// head the operator is keeping, so the journey asserts against live Git state
// rather than restating what the fixture built.
func wedgedFixtureGateAndHead(t *testing.T, p *paths.Paths, runID, local string) (string, string) {
	t.Helper()
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(run.RepoID)
	if _, err := os.Stat(filepath.Join(gateDir, "HEAD")); err != nil {
		t.Fatalf("fixture gate is not a repository: %v", err)
	}
	return gateDir, cliGit(t, local, "rev-parse", "HEAD")
}
