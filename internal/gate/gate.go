package gate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const remoteName = "no-mistakes"

// repoID generates a deterministic 12-char hex ID from an absolute path.
func repoID(absPath string) string {
	h := sha256.Sum256([]byte(absPath))
	return fmt.Sprintf("%x", h[:6])
}

// Init sets up a no-mistakes gate for the git repo at workDir.
// It creates a bare repo, installs the post-receive hook, adds the
// no-mistakes remote, and records the repo in the database.
func Init(ctx context.Context, d *db.DB, p *paths.Paths, workDir string) (*db.Repo, error) {
	// Find git root (resolves symlinks for consistency on macOS).
	gitRoot, err := git.FindGitRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("find git root: %w", err)
	}
	absRoot := gitRoot

	// Check if already initialized.
	existing, err := d.GetRepoByPath(absRoot)
	if err != nil {
		return nil, fmt.Errorf("check existing: %w", err)
	}
	if existing != nil {
		return nil, fmt.Errorf("already initialized for %s", absRoot)
	}

	// Read origin URL.
	upstreamURL, err := git.GetRemoteURL(ctx, absRoot, "origin")
	if err != nil {
		return nil, fmt.Errorf("get origin url: %w", err)
	}

	// Generate deterministic repo ID.
	id := repoID(absRoot)

	// Create bare repo.
	bareDir := p.RepoDir(id)
	if err := git.InitBare(ctx, bareDir); err != nil {
		return nil, fmt.Errorf("create bare repo: %w", err)
	}

	// Install post-receive hook.
	if err := git.InstallPostReceiveHook(bareDir); err != nil {
		// Rollback: remove bare repo.
		os.RemoveAll(bareDir)
		return nil, fmt.Errorf("install hook: %w", err)
	}

	// Record upstream as origin on the gate repo so gh can resolve repository context
	// from detached worktrees created from the gate.
	if err := git.AddRemote(ctx, bareDir, "origin", upstreamURL); err != nil {
		os.RemoveAll(bareDir)
		return nil, fmt.Errorf("add gate origin remote: %w", err)
	}

	// Add remote to working repo.
	if err := git.AddRemote(ctx, absRoot, remoteName, bareDir); err != nil {
		os.RemoveAll(bareDir)
		return nil, fmt.Errorf("add remote: %w", err)
	}

	// Detect default branch from upstream remote.
	branch := git.DefaultBranch(ctx, absRoot, "origin")

	// Insert repo record with deterministic ID.
	repo, err := d.InsertRepoWithID(id, absRoot, upstreamURL, branch)
	if err != nil {
		// Rollback: remove remote and bare repo.
		git.RemoveRemote(ctx, absRoot, remoteName)
		os.RemoveAll(bareDir)
		return nil, fmt.Errorf("insert repo: %w", err)
	}

	slog.Info("gate initialized", "repo_id", id, "path", absRoot, "upstream", upstreamURL)
	return repo, nil
}

// Eject removes the no-mistakes gate from the repo at workDir.
// It removes the remote, deletes the bare repo and worktrees,
// and deletes the repo record from the database.
func Eject(ctx context.Context, d *db.DB, p *paths.Paths, workDir string) (*db.Repo, error) {
	// Find git root (resolves symlinks for consistency on macOS).
	gitRoot, err := git.FindGitRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("find git root: %w", err)
	}
	absRoot := gitRoot

	// Look up repo in DB.
	repo, err := d.GetRepoByPath(absRoot)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("not initialized for %s", absRoot)
	}

	// Remove remote from working repo (non-fatal).
	_ = git.RemoveRemote(ctx, absRoot, remoteName)

	// Delete bare repo.
	bareDir := p.RepoDir(repo.ID)
	os.RemoveAll(bareDir)

	// Delete worktrees for this repo.
	repoWtDir := filepath.Join(p.WorktreesDir(), repo.ID)
	os.RemoveAll(repoWtDir)

	// Delete repo record (cascades to runs + steps).
	if err := d.DeleteRepo(repo.ID); err != nil {
		return nil, fmt.Errorf("delete repo record: %w", err)
	}

	slog.Info("gate ejected", "repo_id", repo.ID, "path", absRoot)
	return repo, nil
}
