//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise the installed hooks with the real CLI and daemon. A deletion
// notification reaches the daemon without starting another pipeline.
func TestEnrolledReceiveHooksExecutableIsolation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	binDir := t.TempDir()
	ownedBin := filepath.Join(binDir, "owner's no-mistakes")
	compiled, err := os.ReadFile(h.NMBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownedBin, compiled, 0o755); err != nil {
		t.Fatal(err)
	}
	h.NMBin = ownedBin
	out, err := h.Run("init", "--install-skill=false")
	if err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}
	t.Logf("CLI enrollment:\n%s", out)
	gate := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	aliasDir, err := os.MkdirTemp("", "nm-alias-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(aliasDir) })
	rootAlias := filepath.Join(aliasDir, "root")
	if err := os.Symlink(h.NMHome, rootAlias); err != nil {
		t.Fatal(err)
	}
	out, err = h.RunInDirWithEnv(h.WorkDir, map[string]string{"NM_HOME": rootAlias}, "init", "--install-skill=false")
	if err != nil {
		t.Fatalf("refresh through symlinked runtime: %v\n%s", err, out)
	}
	t.Logf("CLI enrollment refreshed through symlinked runtime:\n%s", out)
	alias := filepath.Join(t.TempDir(), "gate-alias")
	if err := os.Symlink(gate, alias); err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	poison := t.TempDir()
	marker := filepath.Join(poison, "executed")
	for _, name := range []string{"no-mistakes", "git", "cygpath", "pwd"} {
		body := "#!/bin/sh\n: > " + shellQuote(marker) + "\nexit 99\n"
		if err := os.WriteFile(filepath.Join(poison, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	input := strings.Repeat("1", 40) + " " + strings.Repeat("0", 40) + " refs/heads/deleted\n"
	poisonPWD := false
	runHook := func(receiving, kind, gitDir string) (string, error) {
		cmd := exec.Command("/bin/sh", filepath.Join(receiving, "hooks", kind+"-receive"))
		cmd.Dir = receiving
		pwd := receiving
		if poisonPWD {
			pwd = "."
		}
		cmd.Env = mergedEnv(os.Environ(), map[string]string{
			"NM_HOME": foreign,
			"PATH":    poison + string(os.PathListSeparator) + os.Getenv("PATH"),
			"PWD":     pwd,
			"GIT_DIR": gitDir,
		})
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		t.Logf("%s-receive: receiving=%s GIT_DIR=%q exit=%v\n%s", kind, receiving, gitDir, err, out)
		return string(out), err
	}
	for _, receiving := range []string{gate, alias} {
		if out, err := runHook(receiving, "pre", "."); err != nil {
			t.Fatalf("owner admission failed: %v\n%s", err, out)
		}
		out, err := runHook(receiving, "post", ".")
		if err != nil || !strings.Contains(out, "ref deletion push, no pipeline to run") {
			t.Fatalf("notification did not reach the owning daemon: %v\n%s", err, out)
		}
	}
	if entries, err := os.ReadDir(foreign); err != nil || len(entries) != 0 {
		t.Fatalf("foreign runtime was modified: %v: %v", entries, err)
	}
	t.Log("Owning daemon admitted both physical and symlinked gates; deletion notifications reached it; foreign runtime stayed empty.")
	poisonPWD = true
	if out, err := runHook(gate, "pre", "."); err != nil {
		t.Errorf("relative GIT_DIR with poisoned PWD rejected the owning gate: %v\n%s", err, out)
	}
	if out, err := runHook(gate, "post", "."); err != nil || !strings.Contains(out, "ref deletion push, no pipeline to run") {
		t.Errorf("relative GIT_DIR with poisoned PWD did not notify the owning daemon: %v\n%s", err, out)
	}
	poisonPWD = false
	otherGate := filepath.Join(t.TempDir(), "repos", "copied.git")
	if out, err := h.runGit(context.Background(), h.WorkDir, "init", "--bare", otherGate); err != nil {
		t.Fatalf("create receiving fixture: %v\n%s", err, out)
	}
	for _, kind := range []string{"pre", "post"} {
		content, err := os.ReadFile(filepath.Join(gate, "hooks", kind+"-receive"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(otherGate, "hooks", kind+"-receive"), content, 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := runHook(otherGate, kind, ".")
		if !strings.Contains(out, "does not match enrolled gate") || (kind == "pre" && err == nil) || (kind == "post" && err != nil) {
			t.Fatalf("copied %s hook failed its ownership contract: %v\n%s", kind, err, out)
		}
	}
	log, err := os.ReadFile(filepath.Join(otherGate, "notify-push.log"))
	if err != nil || !strings.Contains(string(log), "does not match enrolled gate") {
		t.Fatalf("copied post-receive failure missing from receiving gate: %v\n%s", err, log)
	}
	t.Logf("Receiving gate's durable copied-hook failure:\n%s", log)
	for _, kind := range []string{"pre", "post"} {
		out, err := runHook(gate, kind, "")
		if !strings.Contains(out, "GIT_DIR is unavailable") || (kind == "pre" && err == nil) || (kind == "post" && err != nil) {
			t.Fatalf("unresolved %s hook did not fail closed: %v\n%s", kind, err, out)
		}
	}
	if err := os.Rename(ownedBin, ownedBin+".removed"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Rename(ownedBin+".removed", ownedBin); err != nil {
			t.Error(err)
		}
	}()
	for _, kind := range []string{"pre", "post"} {
		out, err := runHook(gate, kind, ".")
		if !strings.Contains(out, ownedBin) || (kind == "pre" && err == nil) || (kind == "post" && err != nil) {
			t.Fatalf("missing executable's %s hook did not fail closed: %v\n%s", kind, err, out)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("hook executed a PATH-selected replacement: %v", err)
	}
	t.Log("No PATH-selected replacement ran, including after the enrolled executable was removed.")
	log, err = os.ReadFile(filepath.Join(gate, "notify-push.log"))
	if err != nil || !strings.Contains(string(log), ownedBin) || !strings.Contains(string(log), "GIT_DIR is unavailable") {
		t.Fatalf("notification failures were not durably recorded: %v\n%s", err, log)
	}
	t.Log("The owning gate's notification log retained both unresolved-gate and missing-executable failures.")
	if runs := h.Runs(); len(runs) != 0 {
		t.Fatalf("hook checks unexpectedly created pipeline runs: %v", runs)
	}
	t.Log("The isolated daemon has no pipeline runs after these hook checks.")
}
