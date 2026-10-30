//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	claudeSkillRel = filepath.Join(".claude", "skills", "no-mistakes", "SKILL.md")
	agentsSkillRel = filepath.Join(".agents", "skills", "no-mistakes", "SKILL.md")

	// staleSkillTime is far in the past so that any write init makes to a seeded
	// skill file, even one that rewrites identical bytes, moves its mtime.
	staleSkillTime = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
)

// TestInitSkipUserSkill proves `init --skip-user-skill` never touches the
// user-level skill locations, however the home directory lays them out, while
// still setting up and repairing the gate and saying that the skill was skipped.
//
// Every seeded copy is stale (its bytes differ from what an install would
// write), carries a restrictive mode, and has an old mtime. An install that ran
// would change the bytes, and one that rewrote identical bytes would still move
// the mtime. These snapshots check observable preservation; the implementation
// must bypass installation rather than install and restore a snapshot.
// Absent targets are covered by the snapshot too: a created directory, link
// target or file shows up as a new entry.
func TestInitSkipUserSkill(t *testing.T) {
	layouts := []struct {
		name  string
		setup func(t *testing.T, home string)
	}{
		{"distinct_targets", func(t *testing.T, home string) {
			seedUserSkill(t, home, claudeSkillRel, 0o640, "claude copy")
			seedUserSkill(t, home, agentsSkillRel, 0o600, "agents copy")
		}},
		{"claude_skills_link", func(t *testing.T, home string) {
			seedUserSkill(t, home, agentsSkillRel, 0o640, "shared copy")
			mkdirAll(t, filepath.Join(home, ".claude"))
			symlink(t, filepath.Join("..", ".agents", "skills"), filepath.Join(home, ".claude", "skills"))
		}},
		{"claude_dir_link", func(t *testing.T, home string) {
			seedUserSkill(t, home, agentsSkillRel, 0o640, "shared copy")
			symlink(t, ".agents", filepath.Join(home, ".claude"))
		}},
		{"agents_skills_link", func(t *testing.T, home string) {
			seedUserSkill(t, home, claudeSkillRel, 0o640, "shared copy")
			mkdirAll(t, filepath.Join(home, ".agents"))
			symlink(t, filepath.Join("..", ".claude", "skills"), filepath.Join(home, ".agents", "skills"))
		}},
		{"agents_dir_link", func(t *testing.T, home string) {
			seedUserSkill(t, home, claudeSkillRel, 0o640, "shared copy")
			symlink(t, ".claude", filepath.Join(home, ".agents"))
		}},
		{"dangling_skills_link", func(t *testing.T, home string) {
			mkdirAll(t, filepath.Join(home, ".claude"))
			symlink(t, filepath.Join("..", ".agents", "skills"), filepath.Join(home, ".claude", "skills"))
		}},
		{"one_target_with_sibling_skill", func(t *testing.T, home string) {
			seedUserSkill(t, home, claudeSkillRel, 0o640, "claude copy")
			seedUserSkill(t, home, filepath.Join(".agents", "skills", "other", "SKILL.md"), 0o644, "unrelated skill")
		}},
		{"nothing_present", func(t *testing.T, home string) {}},
	}

	for _, tc := range layouts {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			t.Setenv("USERPROFILE", h.HomeDir)
			tc.setup(t, h.HomeDir)
			before := snapshotSkillHome(t, h.HomeDir)
			identities := logicalSkillFiles(t, h.HomeDir)

			first := initSkippingSkill(t, h)
			if !strings.Contains(first, "Gate initialized") {
				t.Errorf("first init should still report a fresh gate, got:\n%s", first)
			}
			assertSkillHomeUnchanged(t, h.HomeDir, before, identities)

			// Knock the gate remote out so the re-run has something to repair:
			// skipping the skill must not skip the gate refresh.
			ctx := context.Background()
			urlOut, err := h.runGit(ctx, h.WorkDir, "remote", "get-url", "no-mistakes")
			if err != nil {
				t.Fatalf("get gate url: %v\n%s", err, urlOut)
			}
			if out, err := h.runGit(ctx, h.WorkDir, "remote", "remove", "no-mistakes"); err != nil {
				t.Fatalf("remove gate remote: %v\n%s", err, out)
			}

			second := initSkippingSkill(t, h)
			if !strings.Contains(second, "already initialized") {
				t.Errorf("re-init should report an existing gate, got:\n%s", second)
			}
			repaired, err := h.runGit(ctx, h.WorkDir, "remote", "get-url", "no-mistakes")
			if err != nil {
				t.Fatalf("no-mistakes remote not repaired by re-init: %v\n%s", err, repaired)
			}
			if got, want := strings.TrimSpace(string(repaired)), strings.TrimSpace(string(urlOut)); got != want {
				t.Errorf("repaired gate url = %q, want %q", got, want)
			}
			assertSkillHomeUnchanged(t, h.HomeDir, before, identities)
		})
	}
}

// TestInitSkipUserSkillKeepsConcurrentWrite proves a legitimate write to a
// user-level skill file that lands while init is running survives it. A `git`
// shim on PATH holds init at its first `git remote add`, so the write happens
// at a known point between init's start and its end, with no sleeps or polling.
// The default-install case is the control: the same write is overwritten when
// the install runs, which shows the pause really sits before the point where an
// install would act.
func TestInitSkipUserSkillKeepsConcurrentWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell shim and named pipes")
	}

	cases := []struct {
		name     string
		args     []string
		wantKept bool
	}{
		{"default_install_overwrites", []string{"init"}, false},
		{"skip_keeps_write", []string{"init", "--skip-user-skill"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			t.Setenv("USERPROFILE", h.HomeDir)
			seedUserSkill(t, h.HomeDir, claudeSkillRel, 0o640, "claude copy")
			seedUserSkill(t, h.HomeDir, agentsSkillRel, 0o600, "agents copy")
			agentsBefore := snapshotSkillHome(t, h.HomeDir)[agentsSkillRel]

			// Under the harness root, which is removed after the daemon shutdown.
			pause := newGitPause(t, filepath.Join(filepath.Dir(h.WorkDir), "pause"))

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			cmd := exec.CommandContext(ctx, h.NMBin, tc.args...)
			cmd.Dir = h.WorkDir
			cmd.Env = mergedEnv(os.Environ(), map[string]string{
				"PATH": pause.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			})
			cmd.WaitDelay = 5 * time.Second
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Start(); err != nil {
				cancel()
				t.Fatalf("start init: %v", err)
			}
			finished := make(chan struct{})
			var waitErr error
			go func() {
				waitErr = cmd.Wait()
				close(finished)
			}()
			arrived := pause.awaitArrival()
			// Runs before the pipes close and before the harness shuts down: let
			// the shim go, stop init if it is still running, and join everything
			// this test started. out is only read after finished is closed.
			t.Cleanup(func() {
				pause.unblock(t)
				cancel()
				<-finished
				pause.readers.Wait()
				h.syncDaemonOwnership()
			})

			select {
			case err := <-arrived:
				if err != nil {
					t.Fatalf("wait for init to reach git remote add: %v", err)
				}
			case <-finished:
				t.Fatalf("init exited (%v) while waiting for it to reach git remote add:\n%s", waitErr, out.String())
			}

			// Init is paused mid-run: this write lands strictly between its
			// start and its end.
			claudeSkill := filepath.Join(h.HomeDir, claudeSkillRel)
			edited := []byte("---\nname: no-mistakes\n---\nlocal edit made while init was running\n")
			if err := os.WriteFile(claudeSkill, edited, 0o640); err != nil {
				t.Fatalf("write during init: %v", err)
			}
			if err := pause.releaseShim(); err != nil {
				t.Fatalf("release init: %v", err)
			}

			<-finished
			if waitErr != nil {
				t.Fatalf("init: %v\n%s", waitErr, out.String())
			}

			got, err := os.ReadFile(claudeSkill)
			if err != nil {
				t.Fatalf("read skill after init: %v", err)
			}
			if kept := bytes.Equal(got, edited); kept != tc.wantKept {
				t.Errorf("write made during init kept = %v, want %v; file now:\n%s\ninit output:\n%s", kept, tc.wantKept, got, out.String())
			}
			if tc.wantKept {
				if !reportsSkippedSkill(out.String()) {
					t.Errorf("init output should say the skill was skipped, got:\n%s", out.String())
				}
				if after := snapshotSkillHome(t, h.HomeDir)[agentsSkillRel]; after != agentsBefore {
					t.Errorf("the other skill target changed:\n before: %s\n after:  %s", agentsBefore, after)
				}
			} else {
				assertSkillInstalled(t, h)
			}
		})
	}
}

// gitPause is a `git` shim that stops the first `git remote add` made by any
// process with binDir first on PATH, tells the test it arrived, and waits to be
// released before running the real git.
//
// The test opens both named pipes read-write and keeps them open until its
// cleanups have run. That is what keeps every step bounded: neither side can
// block opening a pipe whose peer is gone, a release written to a shim that has
// already exited just sits in the pipe, and the one goroutine that reads the
// arrival pipe is woken by the test's own write instead of waiting on a peer.
// Deadlines are not an option, since Go cannot poll FIFOs on Darwin.
//
// If the test process itself is killed, the kernel closes its ends of the pipes
// and a shim already waiting sees end-of-file and carries on.
type gitPause struct {
	binDir  string
	reached *os.File
	release *os.File
	readers sync.WaitGroup
}

func newGitPause(t *testing.T, dir string) *gitPause {
	t.Helper()
	p := &gitPause{binDir: filepath.Join(dir, "bin")}
	mkdirAll(t, p.binDir)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	var pipes [2]*os.File
	for i, name := range []string{"reached", "release"} {
		path := filepath.Join(dir, name)
		if out, err := exec.Command("mkfifo", path).CombinedOutput(); err != nil {
			t.Fatalf("mkfifo %s: %v\n%s", path, err, out)
		}
		if pipes[i], err = os.OpenFile(path, os.O_RDWR, 0); err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		pipe := pipes[i]
		t.Cleanup(func() { pipe.Close() })
	}
	p.reached, p.release = pipes[0], pipes[1]

	shim := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "remote" ] && [ "$2" = "add" ] && mkdir %s 2>/dev/null; then
  echo reached > %s
  read line < %s
fi
exec %s "$@"
`, shellQuote(filepath.Join(dir, "fired")), shellQuote(p.reached.Name()), shellQuote(p.release.Name()), shellQuote(realGit))
	if err := os.WriteFile(filepath.Join(p.binDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	return p
}

// awaitArrival starts the single reader of the arrival pipe and returns the
// channel its result arrives on.
func (p *gitPause) awaitArrival() <-chan error {
	arrived := make(chan error, 1)
	p.readers.Add(1)
	go func() {
		defer p.readers.Done()
		var b [1]byte
		_, err := p.reached.Read(b[:])
		arrived <- err
	}()
	return arrived
}

// releaseShim lets a waiting shim carry on. It never blocks: the pipe has a
// reader (this test) whether or not the shim is still alive.
func (p *gitPause) releaseShim() error {
	_, err := p.release.Write([]byte("go\n"))
	return err
}

// unblock frees everything that could still be parked on the pipes: a shim
// waiting for its release, and the arrival reader if nothing ever arrived.
func (p *gitPause) unblock(t *testing.T) {
	t.Helper()
	if err := p.releaseShim(); err != nil {
		t.Errorf("release git shim: %v", err)
	}
	if _, err := p.reached.Write([]byte{0}); err != nil {
		t.Errorf("wake arrival reader: %v", err)
	}
}

// initSkippingSkill runs `init --skip-user-skill` in the harness repo and
// checks the output identifies the flag as the reason the skill was skipped
// rather than installed.
func initSkippingSkill(t *testing.T, h *Harness) string {
	t.Helper()
	out, err := h.RunInDir(h.WorkDir, "init", "--skip-user-skill")
	if err != nil {
		t.Fatalf("init --skip-user-skill: %v\n%s", err, out)
	}
	if !reportsSkippedSkill(out) {
		t.Errorf("init output should say the skill was skipped, got:\n%s", out)
	}
	if strings.Contains(out, "installed for agents at user level") {
		t.Errorf("init output must not claim the skill was installed, got:\n%s", out)
	}
	return out
}

func reportsSkippedSkill(out string) bool {
	for _, line := range strings.Split(strings.ToLower(out), "\n") {
		if strings.Contains(line, "skill") && strings.Contains(line, "skip") && strings.Contains(line, "--skip-user-skill") {
			return true
		}
	}
	return false
}

// seedUserSkill writes a stale user-level skill copy at home/rel with the given
// mode and the shared stale mtime, creating parent directories (through any
// symlink already in place).
func seedUserSkill(t *testing.T, home, rel string, perm os.FileMode, label string) {
	t.Helper()
	path := filepath.Join(home, rel)
	mkdirAll(t, filepath.Dir(path))
	body := "---\nname: no-mistakes\n---\nuser copy: " + label + "\n"
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, staleSkillTime, staleSkillTime); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}
}

// logicalSkillFiles returns the file identity behind each logical skill path
// that exists, following symlinks.
func logicalSkillFiles(t *testing.T, home string) map[string]os.FileInfo {
	t.Helper()
	files := map[string]os.FileInfo{}
	for _, rel := range []string{claudeSkillRel, agentsSkillRel} {
		if info, err := os.Stat(filepath.Join(home, rel)); err == nil {
			files[rel] = info
		}
	}
	return files
}

// snapshotSkillHome describes everything under the two user-level skill roots
// as a reader sees it: symlinks are recorded with their target and then
// followed, regular files carry their bytes, mode and mtime, and a root that
// does not exist is recorded as absent so that creating it is a change.
func snapshotSkillHome(t *testing.T, home string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	for _, root := range []string{".claude", ".agents"} {
		snapshotSkillEntry(t, home, root, snap, 0)
	}
	return snap
}

func snapshotSkillEntry(t *testing.T, home, rel string, snap map[string]string, depth int) {
	t.Helper()
	if depth > 8 {
		t.Fatalf("skill home nests deeper than any fixture: %s", rel)
	}
	path := filepath.Join(home, rel)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		snap[rel] = "absent"
		return
	}
	if err != nil {
		t.Fatalf("lstat %s: %v", rel, err)
	}
	desc := ""
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			t.Fatalf("readlink %s: %v", rel, err)
		}
		desc = "symlink to " + target + ", "
		if info, err = os.Stat(path); err != nil {
			snap[rel] = desc + "dangling"
			return
		}
	}
	if info.IsDir() {
		snap[rel] = desc + "dir " + info.Mode().Perm().String()
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatalf("read dir %s: %v", rel, err)
		}
		for _, entry := range entries {
			snapshotSkillEntry(t, home, filepath.Join(rel, entry.Name()), snap, depth+1)
		}
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	snap[rel] = fmt.Sprintf("%sfile %s mtime=%d bytes=%q", desc, info.Mode().Perm(), info.ModTime().UnixNano(), data)
}

func assertSkillHomeUnchanged(t *testing.T, home string, before map[string]string, identities map[string]os.FileInfo) {
	t.Helper()
	after := snapshotSkillHome(t, home)
	for rel, was := range before {
		if now := after[rel]; now != was {
			t.Errorf("init changed %s:\n before: %s\n after:  %s", rel, was, now)
		}
	}
	for rel, now := range after {
		if _, existed := before[rel]; !existed {
			t.Errorf("init created %s: %s", rel, now)
		}
	}
	for rel, was := range identities {
		now, err := os.Stat(filepath.Join(home, rel))
		if err != nil {
			t.Errorf("skill file %s is gone after init: %v", rel, err)
			continue
		}
		if !os.SameFile(was, now) {
			t.Errorf("skill file %s was replaced by a different file", rel)
		}
	}
}
