package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSyncRewordedRecoversUnpublishedFixesOnBothSurfaces(t *testing.T) {
	for _, axi := range []bool{false, true} {
		name := "human"
		if axi {
			name = "axi"
		}
		t.Run(name, func(t *testing.T) {
			f := newCLIRecoverFixture(t)
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := d.UpdateRunErrorStatus(f.runID, "review fixer reworded commits", types.RunFailed); err != nil {
				t.Fatal(err)
			}
			cliGit(t, f.gate, "config", "user.name", "Test")
			cliGit(t, f.gate, "config", "user.email", "test@example.com")
			tree := cliGit(t, f.gate, "rev-parse", f.preserved+"^{tree}")
			rewritten := cliGit(t, f.gate, "commit-tree", tree, "-p", f.submitted, "-m", "fix: reword correction")
			cliGit(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted)
			if out, err := executeCmd("axi", "sync", "--recover"); err == nil || !strings.Contains(out, "blocked_recover_unverified_head") {
				t.Fatalf("baseline did not reproduce: %v\n%s", err, out)
			}
			args := []string{"sync", "--recover", "--observed-head", f.preserved, "--reworded-head", rewritten}
			if axi {
				args = append([]string{"axi"}, args...)
			}
			out, err := executeCmd(args...)
			if err != nil || !strings.Contains(out, "recovered: true") {
				t.Fatalf("recovery: %v\n%s", err, out)
			}
			if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != rewritten {
				t.Fatalf("HEAD = %s", got)
			}
			if got := cliGit(t, f.local, "show", "HEAD:fix.txt"); got != "fix" {
				t.Fatalf("fix lost: %q", got)
			}
			if got := cliGit(t, f.gate, "rev-parse", custody.RewordRef(f.runID, f.preserved)); got != f.preserved {
				t.Fatal("original history lost")
			}
			run, err := d.GetRun(f.runID)
			if err != nil || run.CustodyReturnedAt == nil || run.ReviewApprovedHeadSHA != nil {
				t.Fatalf("bad custody result: %+v %v", run, err)
			}
		})
	}
}
