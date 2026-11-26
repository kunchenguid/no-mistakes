//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This journey needs no agent or daemon: a terminal run's records and real Git
// objects reproduce the recovery boundary, and every action uses the binary.
func TestAxiUnpinnedTerminalHeadRecoveryJourney(t *testing.T) {
	nmBin, _ := buildBinaries(t)
	for _, shape := range []string{
		"pre-push", "published", "dirty", "unrecorded", "missing",
		"unverified", "third-head", "conflicting-anchor", "symbolic-anchor",
	} {
		t.Run(shape, func(t *testing.T) {
			root := t.TempDir()
			p := paths.WithRoot(filepath.Join(root, "nmhome"))
			t.Setenv("NM_HOME", p.Root())
			t.Setenv("NO_MISTAKES_TELEMETRY", "off")
			t.Setenv("NO_MISTAKES_NO_UPDATE_CHECK", "1")
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "no-global-config"))
			t.Setenv("GIT_CONFIG_COUNT", "0")
			h := &Harness{t: t, NMBin: nmBin, WorkDir: filepath.Join(root, "operator")}
			mustGit := func(dir string, args ...string) string {
				t.Helper()
				out, err := h.runGit(context.Background(), dir, args...)
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			write := func(dir, contents string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			mustGit(root, "init", "-b", "main", h.WorkDir)
			write(h.WorkDir, "base\n")
			mustGit(h.WorkDir, "add", ".")
			mustGit(h.WorkDir, "-c", "commit.gpgsign=false", "commit", "-m", "base")
			base := mustGit(h.WorkDir, "rev-parse", "HEAD")
			branch := "feature/recovery"
			mustGit(h.WorkDir, "checkout", "-b", branch)
			write(h.WorkDir, "submitted\n")
			mustGit(h.WorkDir, "-c", "commit.gpgsign=false", "commit", "-am", "submitted")
			submitted := mustGit(h.WorkDir, "rev-parse", "HEAD")
			remote := filepath.Join(root, "upstream.git")
			mustGit(root, "init", "--bare", remote)
			mustGit(h.WorkDir, "remote", "add", "origin", remote)
			mustGit(h.WorkDir, "push", "origin", "HEAD")
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			registered, err := git.FindGitRoot(h.WorkDir)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := database.InsertRepo(registered, remote, "main")
			if err != nil {
				t.Fatal(err)
			}
			gate := p.RepoDir(repo.ID)
			mustGit(root, "init", "--bare", gate)
			mustGit(h.WorkDir, "push", gate, "HEAD:refs/heads/"+branch)
			writer := filepath.Join(root, "pipeline")
			mustGit(root, "clone", h.WorkDir, writer)
			mustGit(writer, "checkout", "-b", "later", base)
			write(writer, "pipeline\n")
			mustGit(writer, "-c", "commit.gpgsign=false", "commit", "-am", "pipeline work")
			preserved := mustGit(writer, "rev-parse", "HEAD")
			// Both commits change the same base line differently. Neither head
			// contains the other, so ordinary containment cannot hide the bug.
			mustGit(writer, "push", gate, "HEAD:refs/heads/later")
			mustGit(gate, "update-ref", "-d", "refs/heads/later", preserved)
			run, err := database.InsertRun(repo.ID, branch, submitted, base)
			if err != nil {
				t.Fatal(err)
			}
			if shape != "pre-push" {
				err = database.UpdateRunPushBinding(run.ID, db.PushBinding{
					HeadSHA: submitted, TargetKind: "upstream",
					TargetFingerprint: branchsync.TargetFingerprint(remote), Ref: "refs/heads/" + branch,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if shape == "missing" {
				preserved = strings.Repeat("f", len(preserved))
			}
			if shape == "unverified" {
				err = database.UpdateRunHeadSHA(run.ID, preserved)
				if err == nil {
					err = database.UpdateRunStatus(run.ID, types.RunFailed)
				}
			} else {
				err = database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, preserved)
			}
			if err != nil {
				t.Fatal(err)
			}
			anchor := custody.RecoveryRef(run.ID)
			switch shape {
			case "dirty":
				write(h.WorkDir, "uncommitted\n")
			case "unrecorded":
				mustGit(h.WorkDir, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "local work")
			case "third-head":
				tree := mustGit(gate, "rev-parse", submitted+"^{tree}")
				third := mustGit(gate, "commit-tree", tree, "-p", submitted, "-m", "independent gate head")
				mustGit(gate, "update-ref", "refs/heads/"+branch, third, submitted)
			case "conflicting-anchor":
				mustGit(gate, "update-ref", anchor, submitted)
			case "symbolic-anchor":
				mustGit(gate, "symbolic-ref", anchor, "refs/heads/"+branch)
			}
			snapshot := func() string {
				return mustGit(h.WorkDir, "status", "--porcelain=v1") + "\n" +
					mustGit(h.WorkDir, "for-each-ref", "--format=%(refname) %(objectname)") + "\n" +
					mustGit(gate, "for-each-ref", "--format=%(refname) %(objectname)")
			}
			before := snapshot()
			out, checkErr := h.RunInDir(h.WorkDir, "axi", "sync", "--check")
			t.Logf("%s check:\n%s", shape, out)
			if checkErr == nil || !strings.Contains(out, "state: pipeline_owned") {
				t.Fatalf("fixture did not reach terminal pipeline custody: %v\n%s", checkErr, out)
			}
			if after := snapshot(); after != before {
				t.Fatal("inspection changed the worktree or refs")
			}
			fresh, err := database.GetRun(run.ID)
			if err != nil || fresh == nil || fresh.CustodyReturnedAt != nil {
				t.Fatalf("inspection returned custody: %+v, %v", fresh, err)
			}
			if shape != "pre-push" && shape != "published" {
				if strings.Contains(out, "source: available_head") {
					t.Fatalf("unsafe shape offered unpinned recovery:\n%s", out)
				}
				return
			}
			for _, want := range []string{"source: available_head", "code: recover_custody", "command: no-mistakes axi sync --recover --keep-local"} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q:\n%s", want, out)
				}
			}
			human, _ := h.RunInDir(h.WorkDir, "sync", "--check")
			if !strings.Contains(human, "explicit recovery will preserve") || strings.Contains(human, "verified archive") {
				t.Fatalf("human guidance does not describe the available head:\n%s", human)
			}
			if after := snapshot(); after != before {
				t.Fatal("human inspection changed the worktree or refs")
			}
			// Follow the offered command, with no archive, reset, or DB repair.
			out, err = h.RunInDir(h.WorkDir, "axi", "sync", "--recover", "--keep-local")
			if err != nil || !strings.Contains(out, "recovered: true") {
				t.Fatalf("offered recovery: %v\n%s", err, out)
			}
			if got := mustGit(h.WorkDir, "rev-parse", "HEAD"); got != submitted {
				t.Fatalf("local head moved to %s", got)
			}
			if got := mustGit(gate, "rev-parse", anchor); got != preserved {
				t.Fatalf("pipeline work was not preserved: %s", got)
			}
			if got := mustGit(gate, "rev-parse", "refs/heads/"+branch); got != submitted {
				t.Fatalf("gate did not keep the local head: %s", got)
			}
			if got := mustGit(remote, "rev-parse", "refs/heads/"+branch); got != submitted {
				t.Fatalf("remote moved to %s", got)
			}
			fresh, err = database.GetRun(run.ID)
			if err != nil || fresh == nil || fresh.CustodyReturnedAt == nil {
				t.Fatalf("custody was not returned: %+v, %v", fresh, err)
			}
		})
	}
}
