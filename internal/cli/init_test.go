package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestResolveWorktreeRoot(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	workDir := t.TempDir()

	if _, err := resolveWorktreeRoot(p, workDir, "   "); err == nil {
		t.Fatal("expected error for empty --worktree-root")
	}
	abs := filepath.Join(t.TempDir(), "runs")
	got, err := resolveWorktreeRoot(p, workDir, abs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != abs {
		t.Errorf("resolveWorktreeRoot(%q) = %q, want %q", abs, got, abs)
	}
	// A relative value never reaches the config, where the daemon's own
	// working directory would decide what it means.
	relative, err := resolveWorktreeRoot(p, workDir, "runs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(relative) {
		t.Errorf("resolveWorktreeRoot(%q) = %q, want an absolute path", "runs", relative)
	}
}

// The placements the config would accept but that defeat the flag: inside the
// directory no-mistakes already owns, inside the repository being initialized,
// or a path that is not a directory at all.
func TestResolveWorktreeRootRejectsUnusablePlacements(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	repoDir := setupTestRepo(t)

	if _, err := resolveWorktreeRoot(p, repoDir, filepath.Join(p.WorktreesDir(), "runs")); err == nil {
		t.Error("expected error for a root inside the default worktrees directory")
	}
	if _, err := resolveWorktreeRoot(p, repoDir, filepath.Join(repoDir, "runs")); err == nil {
		t.Error("expected error for a root inside the repository being initialized")
	}
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWorktreeRoot(p, repoDir, file); err == nil {
		t.Error("expected error for a root that is not a directory")
	}
}

// A root another checkout already claims is refused while the operator can
// still pick another one: the loader rejects two checkouts sharing a root, and
// the daemon refuses to start on a config it cannot load, so printing the entry
// would hand them a paste that stops the daemon instead of placing anything.
func TestResolveWorktreeRootRefusesRootClaimedByAnotherCheckout(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	repoDir := setupTestRepo(t)
	root := filepath.Join(t.TempDir(), "shared-runs")
	otherCheckout := filepath.Join(t.TempDir(), "other-checkout")
	configYAML := "worktree_roots:\n  " + yamlPath(otherCheckout) + ": " + yamlPath(root) + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveWorktreeRoot(p, repoDir, root)
	if err == nil {
		t.Fatal("expected a refusal for a root another checkout already claims")
	}
	if !strings.Contains(err.Error(), otherCheckout) {
		t.Errorf("refusal %q does not name the checkout that claims the root", err)
	}

	// The same checkout re-initializing with the root it already uses is not a
	// conflict with itself.
	selfConfig := "worktree_roots:\n  " + yamlPath(repoDir) + ": " + yamlPath(root) + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(selfConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWorktreeRoot(p, repoDir, root); err != nil {
		t.Errorf("re-initializing the checkout that already uses this root was refused: %v", err)
	}
}

func TestPrintWorktreeRootGuidancePrintsConfigEntry(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	dir := t.TempDir()
	checkout := filepath.Join(dir, "src", "repo1")
	root := filepath.Join(dir, "work", "repo1-runs")
	printWorktreeRootGuidance(&out, p, checkout, root)

	got := out.String()
	for _, want := range []string{"worktree_roots:", checkout + ": " + root, p.ConfigFile()} {
		if !strings.Contains(got, want) {
			t.Errorf("guidance missing %q, got:\n%s", want, got)
		}
	}
}

func TestPrintWorktreeRootGuidanceReportsExistingEntry(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	checkout := filepath.Join(dir, "src", "repo1")
	root := filepath.Join(dir, "work", "repo1-runs")
	configYAML := "worktree_roots:\n  " + yamlPath(checkout) + ": " + yamlPath(root) + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	// The same directory spelled differently is the same entry.
	printWorktreeRootGuidance(&out, p, checkout+string(filepath.Separator), root)

	got := out.String()
	if !strings.Contains(got, "already configured") {
		t.Errorf("guidance should report the entry is in effect, got:\n%s", got)
	}
	if strings.Contains(got, "worktree_roots:") {
		t.Errorf("guidance should not repeat an entry that is already in effect, got:\n%s", got)
	}
}

// yamlPath quotes a path for YAML so a Windows drive letter is not read as a
// mapping and its separators survive as literal backslashes.
func yamlPath(path string) string {
	return `"` + strings.ReplaceAll(path, `\`, `\\`) + `"`
}
