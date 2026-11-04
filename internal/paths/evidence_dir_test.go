package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveEvidenceRootUsesAUsableSettingAndFallsBack(t *testing.T) {
	root := t.TempDir()
	// TempDir may use a symlink (macOS) or a short Windows path. The
	// resolver returns the canonical spelling of configured directories.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	p := WithRoot(root)
	home := filepath.Join(root, "home")
	dropbox := filepath.Join(home, "Dropbox", "no-mistakes-evidence")
	if err := os.MkdirAll(dropbox, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got, warning := p.ResolveEvidenceRoot("~/Dropbox/no-mistakes-evidence", "")
	if warning != "" {
		t.Fatalf("warning = %q, want none", warning)
	}
	if got != dropbox {
		t.Fatalf("ResolveEvidenceRoot(tilde) = %q, want %q", got, dropbox)
	}

	missing, warning := p.ResolveEvidenceRoot(filepath.Join(root, "missing"), "")
	if missing != p.EvidenceDir() || !strings.Contains(warning, "missing or not writable") {
		t.Fatalf("missing = %q warning %q, want the default and a warning", missing, warning)
	}

	t.Run("unwritable", func(t *testing.T) {
		unwritable := filepath.Join(root, "locked")
		if err := os.Mkdir(unwritable, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(unwritable, 0o755) })
		// Mode bits do not deny writes on Windows or for privileged users.
		if probe, err := os.CreateTemp(unwritable, "probe-*"); err == nil {
			probe.Close()
			os.Remove(probe.Name())
			t.Skip("directory permissions do not deny writes on this platform")
		}
		got, warning := p.ResolveEvidenceRoot(unwritable, "")
		if got != p.EvidenceDir() || !strings.Contains(warning, "missing or not writable") {
			t.Fatalf("unwritable = %q warning %q, want the default and a warning", got, warning)
		}
	})

	// A bad evidence_dir falls back to the app-root default, not to local_root.
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	got, warning = p.ResolveEvidenceRoot(filepath.Join(root, "nope"), other)
	if got != p.EvidenceDir() || warning == "" {
		t.Fatalf("bad evidence_dir = %q warning %q, want the default rather than local_root", got, warning)
	}

	got, warning = p.ResolveEvidenceRoot("", other)
	if got != other || warning != "" {
		t.Fatalf("empty evidence_dir = %q warning %q, want local_root %q", got, warning, other)
	}

	inside := p.WorktreeDir("repo", "run")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	got, warning = p.ResolveEvidenceRoot(inside, "")
	if got != p.EvidenceDir() || !strings.Contains(warning, "worktrees") {
		t.Fatalf("worktree evidence_dir = %q warning %q, want the default", got, warning)
	}
}

func TestResolveEvidenceRootDoesNotMoveAnExistingDirectory(t *testing.T) {
	root := t.TempDir()
	p := WithRoot(root)
	oldRun := filepath.Join(p.EvidenceDir(), "run-1")
	if err := os.MkdirAll(oldRun, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRun, "shot.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(root, "next")
	if err := os.Mkdir(next, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, warning := p.ResolveEvidenceRoot(next, ""); warning != "" {
		t.Fatal(warning)
	}
	if _, err := os.Stat(filepath.Join(oldRun, "shot.png")); err != nil {
		t.Fatalf("existing evidence was moved or removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(next, "run-1")); !os.IsNotExist(err) {
		t.Fatalf("resolver created a run directory under the new root: %v", err)
	}
}
