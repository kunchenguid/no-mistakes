package gate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
)

// RemoteName is the name of the git remote that points to the local gate.
const RemoteName = "no-mistakes"

// repoID generates a deterministic 12-char hex ID from an absolute path.
func repoID(absPath string) string {
	h := sha256.Sum256([]byte(absPath))
	return fmt.Sprintf("%x", h[:6])
}

// Init sets up a no-mistakes gate for the git repo at workDir.
// It creates a bare repo, installs the post-receive hook, best-effort
// isolates the bare repo's hooks path from shared local config writes when
// Git supports config --worktree, adds the no-mistakes remote, and records
// the repo in the database.
//
// Init is idempotent: re-running it on an already-initialized repo repairs and
// refreshes the gate (for example installing a newer hook, picking up hook-path
// isolation, or restoring a missing remote) instead of failing. This includes
// a working directory that was renamed or moved since the gate was created:
// the gate identified by the leftover no-mistakes remote is reattached at the
// new path, preserving its run history. The returned bool reports whether a
// new gate was created (true) or an existing one was refreshed (false).
func Init(ctx context.Context, d *db.DB, p *paths.Paths, workDir string) (*db.Repo, bool, error) {
	return InitWithFork(ctx, d, p, workDir, "")
}

// InitWithFork is Init plus an optional GitHub fork push URL. The origin remote
// remains the parent repository used for PRs. When forkURL is empty, an
// existing fork setting is preserved across idempotent refreshes.
func InitWithFork(ctx context.Context, d *db.DB, p *paths.Paths, workDir, forkURL string) (*db.Repo, bool, error) {
	forkURL = strings.TrimSpace(forkURL)

	// Normalize worktrees back to the main repo root so one repo record works
	// from either the main checkout or any attached worktree.
	gitRoot, err := git.FindMainRepoRoot(workDir)
	if err != nil {
		return nil, false, fmt.Errorf("find git root: %w", err)
	}
	absRoot := gitRoot

	// Look up any existing gate so we know whether this is a fresh init or a
	// refresh, and so we never tear down a working gate on a repair failure.
	existing, err := d.GetRepoByPath(absRoot)
	if err != nil {
		return nil, false, fmt.Errorf("check existing: %w", err)
	}
	if existing == nil {
		// No record at this path, but the repo may have been moved or renamed
		// after init; if so, reattach its existing gate instead of failing on
		// the leftover remote.
		existing, err = reattachRelocatedRepo(ctx, d, p, absRoot)
		if err != nil {
			return nil, false, err
		}
	}

	// Read origin URL. Keep the historical rewritten value for non-fork repos,
	// but preserve the literal parent URL when fork routing is configured.
	getOriginURL := git.GetRemoteURL
	if forkURL != "" || (existing != nil && strings.TrimSpace(existing.ForkURL) != "") {
		getOriginURL = git.GetConfiguredRemoteURL
	}
	upstreamURL, err := getOriginURL(ctx, absRoot, "origin")
	if err != nil {
		// A missing "origin" is a normal state for a fresh `git init` repo, so
		// give an actionable message instead of leaking git plumbing. Only
		// substitute it when origin is genuinely absent; any other git failure
		// keeps its original error.
		hasOrigin, listErr := git.HasRemote(ctx, absRoot, "origin")
		if listErr == nil && !hasOrigin {
			return nil, false, fmt.Errorf(
				"no 'origin' remote in %s\n\n"+
					"no-mistakes pushes your branch and opens a pull request, so it needs a remote to push to.\n"+
					"Add one, then re-run:\n\n"+
					"  git remote add origin <url>",
				absRoot)
		}
		return nil, false, fmt.Errorf("get origin url: %w", err)
	}
	if forkURL != "" {
		if err := validateForkRouting(ctx, upstreamURL, forkURL); err != nil {
			return nil, false, err
		}
	}

	// Redact embedded credentials for everything that is persisted, logged, or
	// surfaced to the user. The bare gate keeps the full credentialled URL on
	// its "origin" remote via provisionGate below so worktrees carved from it
	// can still authenticate pushes; the push step resolves that credential
	// from the worktree at run time instead of trusting the DB copy.
	redactedUpstreamURL := safeurl.Redact(upstreamURL)

	id := repoID(absRoot)
	if existing != nil {
		id = existing.ID
	}
	bareDir := p.RepoDir(id)

	// Provision (or repair) the on-disk gate. This is idempotent.
	if err := provisionGate(ctx, bareDir, absRoot, upstreamURL, p.ReposDir(), existing != nil); err != nil {
		// Only tear down a gate we created in this call; never destroy an
		// already-initialized gate when a repair pass fails.
		if existing == nil {
			if remoteURL, remoteErr := git.GetRemoteURL(ctx, absRoot, RemoteName); remoteErr == nil && remoteURL == bareDir {
				git.RemoveRemote(ctx, absRoot, RemoteName)
			}
			os.RemoveAll(bareDir)
		}
		return nil, false, err
	}

	// Detect default branch from upstream remote.
	branch := git.DefaultBranch(ctx, absRoot, "origin")

	if existing != nil {
		var repo *db.Repo
		if forkURL != "" {
			repo, err = d.UpdateRepoMetadataWithFork(existing.ID, redactedUpstreamURL, forkURL, branch)
		} else {
			repo, err = d.UpdateRepoMetadata(existing.ID, redactedUpstreamURL, branch)
		}
		if err != nil {
			return nil, false, fmt.Errorf("update repo metadata: %w", err)
		}
		slog.Info("gate refreshed", "repo_id", repo.ID, "path", absRoot)
		return repo, false, nil
	}

	// Insert repo record with deterministic ID.
	repo, err := d.InsertRepoWithIDAndFork(id, absRoot, redactedUpstreamURL, forkURL, branch)
	if err != nil {
		// Rollback: remove remote and bare repo.
		git.RemoveRemote(ctx, absRoot, RemoteName)
		os.RemoveAll(bareDir)
		return nil, false, fmt.Errorf("insert repo: %w", err)
	}

	slog.Info("gate initialized", "repo_id", id, "path", absRoot, "upstream", redactedUpstreamURL)
	return repo, true, nil
}

func validateForkRouting(ctx context.Context, upstreamURL, forkURL string) error {
	parentProvider := scm.DetectProviderContext(ctx, upstreamURL)
	forkProvider := scm.DetectProviderContext(ctx, forkURL)
	if parentProvider == scm.ProviderGitHub && forkProvider == scm.ProviderGitHub {
		if github.RepoSlug(upstreamURL) == "" || github.RepoSlug(forkURL) == "" {
			return fmt.Errorf("fork URL routing requires GitHub parent and fork remotes with owner/repo paths")
		}
		return nil
	}
	return fmt.Errorf("fork URL routing is currently supported only for GitHub parent and fork remotes (parent provider: %s, fork provider: %s)", parentProvider, forkProvider)
}

// provisionGate creates or repairs the on-disk gate for a repo: the bare repo,
// its push/hook configuration, hook-path isolation, and the git remotes wiring
// the working repo to the gate and the gate to its upstream. Every step is
// idempotent so this doubles as the repair path for re-running init.
func provisionGate(ctx context.Context, bareDir, absRoot, upstreamURL, reposDir string, refresh bool) error {
	// Create the bare repo. git init --bare is a no-op on an existing one.
	if err := git.InitBare(ctx, bareDir); err != nil {
		return fmt.Errorf("create bare repo: %w", err)
	}
	if _, err := git.Run(ctx, bareDir, "config", "receive.advertisePushOptions", "true"); err != nil {
		return fmt.Errorf("enable push options: %w", err)
	}

	if _, err := git.RefreshManagedPostReceiveHook(bareDir); err != nil {
		return fmt.Errorf("install hook: %w", err)
	}

	// Pin core.hookspath in the bare's per-worktree config so subprocess
	// writes to shared local config (e.g. husky during pnpm install) can't
	// disable the gate hook. See git.IsolateHooksPath for details.
	if err := git.IsolateHooksPath(ctx, bareDir); err != nil {
		return fmt.Errorf("isolate hooks path: %w", err)
	}

	// Record upstream as origin on the gate repo so gh can resolve repository
	// context from detached worktrees created from the gate.
	if err := git.EnsureRemote(ctx, bareDir, "origin", upstreamURL); err != nil {
		return fmt.Errorf("add gate origin remote: %w", err)
	}

	if err := ensureWorkingRemote(ctx, absRoot, bareDir, reposDir, refresh); err != nil {
		return fmt.Errorf("add remote: %w", err)
	}

	return nil
}

func ensureWorkingRemote(ctx context.Context, absRoot, bareDir, reposDir string, refresh bool) error {
	if refresh {
		return git.EnsureRemote(ctx, absRoot, RemoteName, bareDir)
	}
	existingURL, err := git.GetRemoteURL(ctx, absRoot, RemoteName)
	if err != nil {
		return git.AddRemote(ctx, absRoot, RemoteName, bareDir)
	}
	if existingURL == bareDir {
		return nil
	}
	// A leftover remote pointing into our own repos dir is stale gate wiring
	// (e.g. the working directory was copied, or its gate was half-ejected);
	// repoint it. Anything else is a user-managed remote we must not touch.
	if filepath.Dir(existingURL) == reposDir {
		return git.EnsureRemote(ctx, absRoot, RemoteName, bareDir)
	}
	return fmt.Errorf("remote %q already exists with url %q", RemoteName, existingURL)
}

// reattachRelocatedRepo detects a working directory that was renamed or moved
// after init: it carries a no-mistakes remote pointing at a gate in our repos
// dir, but its repo record references the old path. When the old path no
// longer exists, the record is migrated to the new path so the existing gate
// and its run history are reattached. It returns nil when the repo should be
// treated as a fresh init instead: no gate remote, an orphan gate with no
// record, or a copy whose original still exists on disk.
func reattachRelocatedRepo(ctx context.Context, d *db.DB, p *paths.Paths, absRoot string) (*db.Repo, error) {
	remoteURL, err := git.GetRemoteURL(ctx, absRoot, RemoteName)
	if err != nil {
		return nil, nil
	}
	id := strings.TrimSuffix(filepath.Base(remoteURL), ".git")
	if p.RepoDir(id) != remoteURL {
		// Not one of our gate paths; fresh init decides what to do with it.
		return nil, nil
	}
	repo, err := d.GetRepo(id)
	if err != nil {
		return nil, fmt.Errorf("look up relocated repo: %w", err)
	}
	if repo == nil {
		return nil, nil
	}
	if _, err := os.Stat(repo.WorkingPath); err == nil {
		// The recorded checkout still exists, so absRoot is a copy of it, not
		// a move; the copy gets its own gate.
		return nil, nil
	}
	migrated, err := d.UpdateRepoWorkingPath(id, absRoot)
	if err != nil {
		return nil, fmt.Errorf("migrate repo working path: %w", err)
	}
	slog.Info("gate reattached after working dir move", "repo_id", id, "old_path", repo.WorkingPath, "new_path", absRoot)
	return migrated, nil
}

// Eject removes the no-mistakes gate from the repo at workDir.
// It removes the remote, deletes the bare repo and worktrees,
// and deletes the repo record from the database.
func Eject(ctx context.Context, d *db.DB, p *paths.Paths, workDir string) (*db.Repo, error) {
	// Normalize worktrees back to the main repo root so eject works no matter
	// which checkout the user runs it from.
	gitRoot, err := git.FindMainRepoRoot(workDir)
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
	_ = git.RemoveRemote(ctx, absRoot, RemoteName)

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
