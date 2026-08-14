package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/skill"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
	"github.com/spf13/cobra"
)

const banner = `_  _ ____    _  _ _ ____ ___ ____ _  _ ____ ____
|\ | |  |    |\/| | [__   |  |__| |_/  |___ [__
| \| |__|    |  | | ___]  |  |  | | \_ |___ ___]`

func newInitCmd() *cobra.Command {
	var forkURL string
	var worktreeRoot string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize no-mistakes gate for the current repository",
		Long: "Sets up or refreshes a local bare repo as a gate, installs a post-receive hook,\n" +
			"best-effort isolates the gate hook path from shared local git config writes when Git supports `config --worktree`,\n" +
			"adds or repairs the \"no-mistakes\" git remote, and records the repo in the database.\n\n" +
			"Run this from inside a git repository that has an \"origin\" remote.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("init", func() error {
				p, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				if cmd.Flags().Changed("fork-url") && strings.TrimSpace(forkURL) == "" {
					return fmt.Errorf("init: --fork-url must not be empty")
				}
				resolvedWorktreeRoot := ""
				if cmd.Flags().Changed("worktree-root") {
					resolvedWorktreeRoot, err = resolveWorktreeRoot(p, d, ".", worktreeRoot)
					if err != nil {
						return err
					}
				}
				repo, created, err := gate.InitWithFork(cmd.Context(), d, p, ".", forkURL)
				if err != nil {
					return fmt.Errorf("init: %w", err)
				}
				if err := daemon.EnsureDaemon(p); err != nil {
					// Only roll back a gate we created in this run; a re-init
					// must never eject a user's pre-existing gate.
					if created {
						if _, ejectErr := gate.Eject(cmd.Context(), d, p, "."); ejectErr != nil {
							return fmt.Errorf("start daemon: %w, rollback init: %v", err, ejectErr)
						}
					}
					return fmt.Errorf("start daemon: %w", err)
				}

				// Install the agent skill at user level so agents can drive
				// no-mistakes via `/no-mistakes` in any repo. Best-effort: a
				// skill write failure must not undo a successful gate setup.
				_, skillErr := skill.InstallUser()

				w := cmd.OutOrStdout()
				fmt.Fprintln(w, sCyan.Render(banner))
				fmt.Fprintln(w)
				headline := "Gate initialized"
				if !created {
					headline = "Gate already initialized (refreshed)"
				}
				fmt.Fprintf(w, "  %s %s\n", sGreen.Render("✓"), headline)
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  repo"), repo.WorkingPath)
				fmt.Fprintf(w, "  %s  no-mistakes → %s\n", sDim.Render("  gate"), p.RepoDir(repo.ID))
				remoteURL := repo.UpstreamURL
				if repo.ForkURL != "" {
					remoteURL = safeurl.Redact(remoteURL)
				}
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("remote"), remoteURL)
				if repo.ForkURL != "" {
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  fork"), safeurl.Redact(repo.ForkURL))
				}
				if skillErr != nil {
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render(" skill"), sYellow.Render("skipped: "+skillErr.Error()))
				} else {
					fmt.Fprintf(w, "  %s  %s %s\n", sDim.Render(" skill"), sGreen.Render("/no-mistakes"), sDim.Render("installed for agents at user level"))
				}
				if resolvedWorktreeRoot != "" {
					printWorktreeRootGuidance(w, p, repo.WorkingPath, resolvedWorktreeRoot)
				}
				if legacy := skill.Vendored(repo.WorkingPath); len(legacy) > 0 {
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  note"), sDim.Render("vendored skill copy ("+strings.Join(legacy, ", ")+") is no longer needed and can be removed"))
				}
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s\n", sDim.Render("Push through the gate with:"))
				fmt.Fprintf(w, "  %s\n", sBold.Render("git push no-mistakes <branch>"))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&forkURL, "fork-url", "", "GitHub fork remote URL to push branches to while opening PRs against origin")
	cmd.Flags().StringVar(&worktreeRoot, "worktree-root", "", "Directory to create this repository's run worktrees in, so directory-scoped toolchain config (mise, direnv) reaches them; prints the worktree_roots entry to add to the global config")
	return cmd
}

// resolveWorktreeRoot validates a --worktree-root value and returns the
// absolute directory the config entry must name. A relative value is resolved
// against the current directory here rather than accepted into the config,
// where it would be read by a daemon with an unrelated working directory.
//
// It also refuses every placement the daemon refuses to start on, through the
// same owner (worktrees.CheckPlacement) and against the same checkouts: NM_HOME,
// the checkout being initialized, every checkout the config names, and every
// registered repository (db.RepoWorkingPaths, which is where the daemon's startup
// gate reads them too).
//
// Finally it refuses a root another checkout has already claimed, which the
// config loader rejects. All of it belongs here rather than only in the daemon:
// the daemon refuses to start on a config it cannot load or place, and every
// command starts the daemon, so printing such an entry would hand the operator a
// paste that takes their whole CLI down instead of placing anything - the failure
// belongs where they can still pick another directory.
func resolveWorktreeRoot(p *paths.Paths, d *db.DB, workDir, root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("init: --worktree-root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("init: resolve --worktree-root: %w", err)
	}
	abs = filepath.Clean(abs)
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		return "", fmt.Errorf("init: --worktree-root %s is not a directory", abs)
	}
	gitRoot, _ := git.FindMainRepoRoot(workDir)
	if err := worktrees.CheckPlacement(p, gitRoot, abs, knownCheckouts(p, d)...); err != nil {
		return "", fmt.Errorf("init: --worktree-root %w", err)
	}
	if claimant, claimed := checkoutClaimingWorktreeRoot(p, gitRoot, abs); claimed {
		return "", fmt.Errorf("init: --worktree-root %s is already the worktree root of %s; each checkout needs its own root", abs, claimant)
	}
	return abs, nil
}

// knownCheckouts lists every checkout placement validation must protect: the
// ones the global config names plus every registered repository. It is the same
// set the daemon's startup gate judges against, so init cannot accept a
// placement that then stops the daemon.
//
// An unreadable config or database yields whichever half could be read: the
// operator has a different problem, and the daemon reports that one on its own.
func knownCheckouts(p *paths.Paths, d *db.DB) []string {
	var out []string
	if cfg, err := config.LoadGlobal(p.ConfigFile()); err == nil {
		out = append(out, worktrees.New(p, cfg.WorktreeRoots).Checkouts()...)
	}
	if d != nil {
		if registered, err := d.RepoWorkingPaths(); err == nil {
			out = append(out, registered...)
		}
	}
	return out
}

// checkoutClaimingWorktreeRoot reports the configured checkout that already
// places its run worktrees in root, when that checkout is not the one being
// initialized. An unreadable config yields no claim: the operator has a
// different problem, and the daemon reports that one on its own.
func checkoutClaimingWorktreeRoot(p *paths.Paths, checkout, root string) (string, bool) {
	cfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return "", false
	}
	layout := worktrees.New(p, cfg.WorktreeRoots)
	self := worktrees.Canonical(checkout)
	for _, configured := range layout.Checkouts() {
		if checkout != "" && configured == self {
			continue
		}
		if claimed, ok := layout.CustomRoot(configured); ok && worktrees.Canonical(claimed) == worktrees.Canonical(root) {
			return configured, true
		}
	}
	return "", false
}

// printWorktreeRootGuidance reports the worktree_roots entry that places this
// repository's run worktrees in the requested directory. no-mistakes never
// rewrites the global config - it is a hand-maintained file with the
// operator's own comments - so init prints the exact entry to add instead of
// editing it, and says nothing further when the entry is already in effect.
//
// What it prints depends on whether the config already has a worktree_roots
// block, because YAML rejects a duplicate top-level key: an operator who pasted
// a second `worktree_roots:` - the normal outcome for the second repository they
// place - would get a config that no longer loads, and a daemon that refuses to
// start until they repair it by hand. With a block already present, only the
// entry line is printed, to add under the existing key.
//
// Presence is a question about the document, not about the entries it parsed to
// (see config.GlobalConfigHasKey): a key with no value and a key set to {} both
// parse to nothing, and telling the operator to add a second one is the failure
// this branch exists to avoid.
func printWorktreeRootGuidance(w io.Writer, p *paths.Paths, workingPath, root string) {
	if cfg, err := config.LoadGlobal(p.ConfigFile()); err == nil {
		if configured, ok := worktrees.New(p, cfg.WorktreeRoots).CustomRoot(workingPath); ok && worktrees.Canonical(configured) == worktrees.Canonical(root) {
			fmt.Fprintf(w, "  %s  %s %s\n", sDim.Render("  runs"), sGreen.Render(root), sDim.Render("(already configured)"))
			return
		}
	}
	blockPresent := config.GlobalConfigHasKey(p.ConfigFile(), "worktree_roots")
	entry := "  " + workingPath + ": " + root
	fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  runs"), root)
	fmt.Fprintln(w)
	if blockPresent {
		fmt.Fprintf(w, "  %s\n", sDim.Render("Add this under the existing worktree_roots: in "+p.ConfigFile()+" so runs are created there:"))
		fmt.Fprintf(w, "  %s\n", sBold.Render(entry))
		return
	}
	fmt.Fprintf(w, "  %s\n", sDim.Render("Add this to "+p.ConfigFile()+" so runs are created there:"))
	fmt.Fprintf(w, "  %s\n", sBold.Render("worktree_roots:"))
	fmt.Fprintf(w, "  %s\n", sBold.Render(entry))
}
