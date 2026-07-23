package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestMigrateGateConfigsRejectsInvalidDirectoriesAndSkipsCurrentGates(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent-worktree")
	if out, err := exec.Command("git", "init", parent).CombinedOutput(); err != nil {
		t.Fatalf("init parent worktree: %v: %s", err, out)
	}
	parentConfig := filepath.Join(parent, ".git", "config")
	parentConfigBefore, err := os.ReadFile(parentConfig)
	if err != nil {
		t.Fatal(err)
	}

	p := paths.WithRoot(filepath.Join(parent, "scratch", "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	ctx := context.Background()
	registeredID := "registered"
	legacyID := "strict-legacy"
	for _, id := range []string{registeredID, legacyID} {
		if err := git.InitBare(ctx, p.RepoDir(id)); err != nil {
			t.Fatalf("init %s gate: %v", id, err)
		}
	}
	if _, err := database.InsertRepoWithID(registeredID, filepath.Join(parent, "source"), "https://example.com/registered.git", "main"); err != nil {
		t.Fatal(err)
	}

	invalidDirs := []string{
		filepath.Join(p.ReposDir(), ".turbo"),
		filepath.Join(p.ReposDir(), "arbitrary"),
		filepath.Join(p.ReposDir(), "malformed.git"),
	}
	for _, dir := range invalidDirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("do not mutate\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	malformedDir := filepath.Join(p.ReposDir(), "malformed.git")
	if err := os.Mkdir(filepath.Join(malformedDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(malformedDir, "HEAD"), []byte("not-a-valid-head\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertRepoWithID("malformed", filepath.Join(parent, "malformed-source"), "https://example.com/malformed.git", "main"); err != nil {
		t.Fatal(err)
	}

	stats := migrateGateConfigs(ctx, database, p)
	if stats.Gates != 2 || stats.Migrated != 2 || stats.Current != 0 || stats.Rejected != 3 || stats.Failed != 0 {
		t.Fatalf("first migration stats = %+v, want 2 migrated gates and 3 rejected directories", stats)
	}

	for _, dir := range invalidDirs {
		if _, err := os.Stat(filepath.Join(dir, "hooks")); !os.IsNotExist(err) {
			t.Fatalf("invalid directory %s was mutated with hooks: %v", dir, err)
		}
		marker, err := os.ReadFile(filepath.Join(dir, "marker"))
		if err != nil || string(marker) != "do not mutate\n" {
			t.Fatalf("invalid directory marker changed at %s: %q, %v", dir, marker, err)
		}
	}
	parentConfigAfter, err := os.ReadFile(parentConfig)
	if err != nil {
		t.Fatal(err)
	}
	if string(parentConfigAfter) != string(parentConfigBefore) {
		t.Fatalf("migration discovered and mutated the ancestor repository\nbefore:\n%s\nafter:\n%s", parentConfigBefore, parentConfigAfter)
	}
	if _, err := os.Stat(filepath.Join(parent, ".git", "config.worktree")); !os.IsNotExist(err) {
		t.Fatalf("ancestor config.worktree was created: %v", err)
	}

	type snapshot struct {
		content []byte
		mode    os.FileMode
		mtime   int64
	}
	snapshots := make(map[string]snapshot)
	for _, id := range []string{registeredID, legacyID} {
		bareDir := p.RepoDir(id)
		for _, name := range []string{"config", "config.worktree", "hooks/post-receive", "no-mistakes-gate-config"} {
			path := filepath.Join(bareDir, filepath.FromSlash(name))
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read migrated %s: %v", path, err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			snapshots[path] = snapshot{content: content, mode: info.Mode(), mtime: info.ModTime().UnixNano()}
		}
	}

	second := migrateGateConfigs(ctx, database, p)
	if second.Gates != 2 || second.Current != 2 || second.Migrated != 0 || second.Rejected != 3 || second.Failed != 0 {
		t.Fatalf("second migration stats = %+v, want two cheap current gates", second)
	}
	for path, before := range snapshots {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != string(before.content) || info.Mode() != before.mode || info.ModTime().UnixNano() != before.mtime {
			t.Fatalf("current gate file was rewritten on restart: %s", path)
		}
	}
}

func TestMigrateGateConfigsDoesNotStampUnsupportedIsolation(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	ctx := context.Background()
	id := "unsupported"
	if err := git.InitBare(ctx, p.RepoDir(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertRepoWithID(id, t.TempDir(), "https://example.com/unsupported.git", "main"); err != nil {
		t.Fatal(err)
	}

	oldEnsure := ensureGateHooksPathIsolation
	ensureGateHooksPathIsolation = func(context.Context, string) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() { ensureGateHooksPathIsolation = oldEnsure })

	first := migrateGateConfigs(ctx, database, p)
	if first.Gates != 1 || first.Failed != 1 || first.Migrated != 0 || first.Current != 0 {
		t.Fatalf("first migration stats = %+v, want unstamped failure", first)
	}
	if git.GateConfigCurrent(p.RepoDir(id)) {
		t.Fatal("unsupported isolation must not be stamped current")
	}

	second := migrateGateConfigs(ctx, database, p)
	if second.Gates != 1 || second.Failed != 1 || second.Migrated != 0 || second.Current != 0 {
		t.Fatalf("second migration stats = %+v, want migration retry", second)
	}
}
