package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These journeys execute the shipped CLI against real Git repositories and
// SQLite state. Historical runs are seeded; no daemon or agent is substituted
// for the adoption operation under test.
func TestPublishedRebaseAdoptionBinary(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bin := buildNoMistakesBinary(t)
	branchRef := "refs/heads/feature/recover"
	archive := func(f cliRecoverFixture) string {
		return "refs/tags/no-mistakes-abandoned/feature/recover/" + f.submitted
	}
	run := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		out, err := execNoMistakesErr(t, bin, args...)
		t.Logf("$ no-mistakes %s\n%s", strings.Join(args, " "), out)
		return out, err
	}
	snapshot := func(t *testing.T, f cliRecoverFixture) map[string]string {
		t.Helper()
		state := map[string]string{
			"gate refs":   cliGit(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"),
			"remote refs": cliGit(t, f.remote, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"),
			"local refs":  cliGit(t, f.local, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"),
			"local head":  cliGit(t, f.local, "rev-parse", "HEAD"),
			"branch":      cliGit(t, f.local, "branch", "--show-current"),
			"status":      cliGit(t, f.local, "status", "--porcelain=v1"),
			"index":       cliGit(t, f.local, "ls-files", "--stage"),
		}
		for _, name := range []string{"file.txt", "untracked.txt"} {
			data, err := os.ReadFile(filepath.Join(f.local, name))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			state[name] = string(data)
		}
		return state
	}
	withDB := func(t *testing.T, fn func(*db.DB) error) {
		t.Helper()
		d, err := db.Open(paths.WithRoot(os.Getenv("NM_HOME")).DB())
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := fn(d); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name     string
		human    bool
		conflict bool
	}{
		{"structured CLI adopts a published conflict resolution", false, true},
		{"human CLI adopts a published conflict resolution", true, true},
		{"structured CLI adopts a patch-equivalent rebase", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIUnmovedAbortFixture(t)
			var rebased string
			if tc.conflict {
				rebased = rebaseReleasedBranchOverConflict(t, f, true)
			} else {
				cliGit(t, f.local, "remote", "add", "origin", f.remote)
				cliGit(t, f.local, "push", "origin", "HEAD:"+branchRef)
				cliGit(t, f.local, "checkout", "main")
				if err := os.WriteFile(filepath.Join(f.local, "upstream.txt"), []byte("upstream addition\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				cliGit(t, f.local, "add", "upstream.txt")
				cliGit(t, f.local, "commit", "-m", "advance main")
				cliGit(t, f.local, "checkout", "feature/recover")
				cliGit(t, f.local, "rebase", "main")
				cliGit(t, f.local, "push", "--force-with-lease="+branchRef+":"+f.submitted, "origin", "HEAD:"+branchRef)
				rebased = cliGit(t, f.local, "rev-parse", "HEAD")
				if !strings.HasPrefix(cliGit(t, f.local, "cherry", rebased, f.submitted), "- ") {
					t.Fatal("rebase did not preserve patch equivalence")
				}
			}
			status, err := run(t, "axi", "status")
			if err != nil || !strings.Contains(status, "state: user_owned") || strings.Contains(status, "next_action:") {
				t.Fatalf("released branch status: %v\n%s", err, status)
			}
			before := snapshot(t, f)
			args := []string{"axi", "sync", "--adopt-published"}
			if tc.human {
				args = []string{"sync", "--adopt-published", "--yes"}
			}
			out, err := run(t, args...)
			if err != nil {
				t.Fatalf("published adoption: %v\n%s", err, out)
			}
			if got := cliGit(t, f.gate, "rev-parse", branchRef); got != rebased {
				t.Fatalf("gate head = %s, want %s", got, rebased)
			}
			if got := cliGit(t, f.gate, "for-each-ref", "--format=%(objectname) %(symref)", archive(f)); got != f.submitted {
				t.Fatalf("archive = %q, want direct ref at %s", got, f.submitted)
			}
			for key, got := range snapshot(t, f) {
				if key != "gate refs" && got != before[key] {
					t.Fatalf("adoption changed %s: %q -> %q", key, before[key], got)
				}
			}
			withDB(t, func(d *db.DB) error {
				record, err := d.GetRun(f.runID)
				if err == nil && (record == nil || record.CustodyReturnedAt != nil) {
					t.Fatal("adoption altered the released run's custody")
				}
				return err
			})
			t.Logf("Verified: local and remote remain %s; gate adopted %s; direct archive preserves %s; files and index unchanged", rebased, rebased, f.submitted)
			before = snapshot(t, f)
			out, err = run(t, "axi", "sync", "--adopt-published")
			if err == nil || !strings.Contains(out, "already holds the local head") {
				t.Fatalf("repeated adoption: %v\n%s", err, out)
			}
			for key, got := range snapshot(t, f) {
				if got != before[key] {
					t.Fatalf("repeated adoption changed %s", key)
				}
			}
		})
	}

	for _, tc := range []struct {
		name    string
		safety  string
		arrange func(*testing.T, cliRecoverFixture)
	}{
		{"unpublished rewrite", "blocked_published_head_mismatch", func(t *testing.T, f cliRecoverFixture) {
			cliGit(t, f.remote, "update-ref", branchRef, f.submitted)
		}},
		{"missing remote branch", "blocked_published_head_mismatch", func(t *testing.T, f cliRecoverFixture) {
			cliGit(t, f.remote, "update-ref", "-d", branchRef)
		}},
		{"foreign origin cannot prove publication", "blocked_published_head_mismatch", func(t *testing.T, f cliRecoverFixture) {
			foreign := filepath.Join(t.TempDir(), "foreign.git")
			cliGit(t, f.local, "init", "--bare", foreign)
			cliGit(t, f.local, "push", foreign, "HEAD:"+branchRef)
			cliGit(t, f.local, "remote", "set-url", "origin", foreign)
			cliGit(t, f.remote, "update-ref", branchRef, f.submitted)
		}},
		{"configured target unavailable", "blocked_adopt_published_offline", func(t *testing.T, f cliRecoverFixture) {
			withDB(t, func(d *db.DB) error {
				_, err := d.ReplaceRepoURLs(f.repoID, filepath.Join(t.TempDir(), "unavailable.git"), "")
				return err
			})
		}},
		{"dirty tracked file", "blocked_adopt_published_dirty", func(t *testing.T, f cliRecoverFixture) {
			if err := os.WriteFile(filepath.Join(f.local, "file.txt"), []byte("uncommitted work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"staged edit", "blocked_adopt_published_dirty", func(t *testing.T, f cliRecoverFixture) {
			if err := os.WriteFile(filepath.Join(f.local, "file.txt"), []byte("staged work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cliGit(t, f.local, "add", "file.txt")
		}},
		{"untracked file", "blocked_adopt_published_dirty", func(t *testing.T, f cliRecoverFixture) {
			if err := os.WriteFile(filepath.Join(f.local, "untracked.txt"), []byte("unsaved work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"new active owner", "blocked_adopt_published_not_applicable", func(t *testing.T, f cliRecoverFixture) {
			head := cliGit(t, f.local, "rev-parse", "HEAD")
			withDB(t, func(d *db.DB) error {
				r, err := d.InsertRun(f.repoID, "feature/recover", head, f.base)
				if err != nil {
					return err
				}
				return d.UpdateRunStatus(r.ID, types.RunRunning)
			})
		}},
		{"gate head changed", "blocked_adopt_published_gate_changed", func(t *testing.T, f cliRecoverFixture) {
			cliGit(t, f.gate, "update-ref", branchRef, f.base, f.submitted)
		}},
		{"conflicting archive", "blocked_adopt_published_preserve_failed", func(t *testing.T, f cliRecoverFixture) {
			cliGit(t, f.gate, "update-ref", archive(f), f.base)
		}},
		{"symbolic archive", "blocked_adopt_published_preserve_failed", func(t *testing.T, f cliRecoverFixture) {
			cliGit(t, f.gate, "symbolic-ref", archive(f), branchRef)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIUnmovedAbortFixture(t)
			rebaseReleasedBranchOverConflict(t, f, true)
			tc.arrange(t, f)
			before := snapshot(t, f)
			out, err := run(t, "axi", "sync", "--adopt-published")
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(out, "safety: "+tc.safety) {
				t.Fatalf("expected refusal %s with exit 1, got %v\n%s", tc.safety, err, out)
			}
			for key, got := range snapshot(t, f) {
				if got != before[key] {
					t.Fatalf("refusal changed %s: %q -> %q", key, before[key], got)
				}
			}
			t.Logf("Verified refusal %s: all local, gate and remote refs, worktree bytes and index remain unchanged; local head %s", tc.safety, before["local head"])
		})
	}

	t.Run("custody-returned branch still adopts a published rewrite", func(t *testing.T) {
		f := newCLIRecoverFixture(t)
		rebased := rebaseReturnedCustodyBranch(t, f, f.remote, true)
		before := snapshot(t, f)
		out, err := run(t, "axi", "sync", "--adopt-published")
		if err != nil || !strings.Contains(out, "safety: gate_ready") {
			t.Fatalf("custody-returned adoption: %v\n%s", err, out)
		}
		if got := cliGit(t, f.gate, "rev-parse", branchRef); got != rebased {
			t.Fatalf("gate head = %s, want %s", got, rebased)
		}
		if got := cliGit(t, f.gate, "rev-parse", "refs/no-mistakes/recover/"+f.runID); got != f.preserved {
			t.Fatalf("recovery anchor = %s, want %s", got, f.preserved)
		}
		for key, got := range snapshot(t, f) {
			if key != "gate refs" && got != before[key] {
				t.Fatalf("adoption changed %s: %q -> %q", key, before[key], got)
			}
		}
		t.Logf("Verified: gate adopted %s, recovered head %s remains anchored, local and remote refs and files unchanged", rebased, f.preserved)
	})
}
