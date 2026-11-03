package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSyncReconciliationExecutesObservedChoice(t *testing.T) {
	for _, axi := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "axi"}[axi], func(t *testing.T) {
			f := newCLIRecoverFixture(t)
			cliGit(t, f.local, "push", f.remote, "HEAD:refs/heads/feature/recover")
			if err := os.WriteFile(filepath.Join(f.local, "file.txt"), []byte("independent local work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cliGit(t, f.local, "commit", "-am", "local work")
			local := cliGit(t, f.local, "rev-parse", "HEAD")
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if err := database.UpdateRunPushBinding(f.runID, db.PushBinding{HeadSHA: f.submitted, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover"}); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatusWithVerifiedHead(f.runID, types.RunCancelled, f.preserved); err != nil {
				t.Fatal(err)
			}
			out, _ := executeCmd("axi", "sync", "--check")
			match := regexp.MustCompile(`--observed ([a-f0-9]{64})`).FindStringSubmatch(out)
			if len(match) != 2 || !strings.Contains(out, "phase: post_push") || !strings.Contains(out, "code: reconcile_custody") {
				t.Fatalf("missing executable reconciliation offer:\n%s", out)
			}
			args := []string{"sync", "--recover", "--keep", "local", "--observed", match[1]}
			if axi {
				args = append([]string{"axi"}, args...)
			}
			out, err = executeCmd(args...)
			if err != nil {
				t.Fatalf("reconcile: %v\n%s", err, out)
			}
			run, err := database.GetRun(f.runID)
			if err != nil || run.CustodyReturnedAt == nil {
				t.Fatalf("custody not returned: %#v, %v", run, err)
			}
			for _, dir := range []string{f.local, f.gate} {
				if head := cliGit(t, dir, "rev-parse", "refs/heads/feature/recover"); head != local {
					t.Fatalf("selected branch = %s", head)
				}
				if head := cliGit(t, dir, "rev-parse", "refs/no-mistakes/recover/"+f.runID); head != f.preserved {
					t.Fatalf("lost unpublished head: %s", head)
				}
			}
		})
	}
}
