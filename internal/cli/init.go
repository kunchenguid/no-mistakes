package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
				if err := assertCheckoutHoldsNoConfiguredWorktreeRoot(p, "."); err != nil {
					return err
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

// assertCheckoutHoldsNoConfiguredWorktreeRoot refuses to register a checkout
// that contains a directory the configuration already places run worktrees in.
//
// It runs on EVERY init, not only the one that names a directory, because which
// placements are usable is decided by which checkouts exist: worktrees.CheckPlacement
// refuses a root inside ANY registered checkout, and the daemon refuses to start
// while a configured entry is unusable. Registering a checkout AROUND an existing
// root therefore reaches the very state --worktree-root is careful never to
// print, from the other direction - and the operator would find out at the next
// daemon start, when every command goes down with it, from a message naming a
// config entry they did not touch. Refusing here leaves them a checkout they can
// still place elsewhere and an entry they can still repoint.
//
// Only entries this checkout would newly break are judged. An unrelated one that
// is already unusable is the daemon's report to make: it is not this
// registration's doing, and init has no business refusing to run over it.
//
// A config that does not load is refused outright instead, naming the fault. It
// cannot be judged - the entries it would be judged against are the ones that
// did not load - and registering into it is never useful: the daemon refuses to
// start on that same config, so the registration lands in a CLI that is already
// down, and the placement it may have broken then surfaces as a second fault to
// find by hand.
func assertCheckoutHoldsNoConfiguredWorktreeRoot(p *paths.Paths, workDir string) error {
	checkout, err := git.FindMainRepoRoot(workDir)
	if err != nil || strings.TrimSpace(checkout) == "" {
		return nil
	}
	cfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return fmt.Errorf("init: %s must load before a repository can be registered: %w", p.ConfigFile(), err)
	}
	layout := worktrees.New(p, cfg.WorktreeRoots)
	configured := layout.Checkouts()
	sort.Strings(configured)
	for _, other := range configured {
		root, ok := layout.CustomRoot(other)
		if !ok || !worktrees.Contains(checkout, root) {
			continue
		}
		if err := layout.ValidateCheckout(other, checkout); err != nil {
			return fmt.Errorf("init: registering %s makes the configured worktree placement unusable: %w", checkout, err)
		}
	}
	return nil
}

// knownCheckouts lists every checkout placement validation must protect: the
// ones the global config names plus every registered repository. It is the same
// set the daemon's startup gate judges against, so init cannot accept a
// placement that then stops the daemon.
//
// An unreadable database yields the config half alone, which is the whole set on
// a machine that has registered nothing. An unreadable config cannot get here:
// init refuses one before it registers anything (see
// assertCheckoutHoldsNoConfiguredWorktreeRoot).
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
// initialized. An unreadable config yields no claim, and cannot get here anyway:
// init refuses one before it registers anything (see
// assertCheckoutHoldsNoConfiguredWorktreeRoot).
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
// What it prints is whatever edit leaves the config loadable, because YAML
// rejects a duplicate key at either level and a config that no longer loads is a
// daemon that refuses to start - every command runs EnsureDaemon, so the
// operator's whole CLI goes with it. Four edits, four shapes:
//
//   - No key: the key and the entry.
//   - The key is there but nothing can be added under it - `worktree_roots: {}`,
//     `worktree_roots: {a: b}`, or a valueless `worktree_roots:`: the whole key
//     is replaced by the block form, carrying the entries it already had. An
//     indented entry line after a flow mapping is not part of that mapping at
//     all; YAML rejects the document.
//   - A block, and this checkout already has a DIFFERENT root: its key is
//     already in the block, so the edit is to replace that entry's value. Adding
//     a second `<checkout>:` under the same block is the duplicate-key failure
//     one level down from a second `worktree_roots:`. Both lines are printed as
//     the config spells them, so the operator can find the one to replace.
//   - A block without this checkout: only the entry line, to add under the
//     existing key.
//
// Which of those applies is a question about the document rather than about the
// entries it parsed to (see config.InspectGlobalConfigMapping): a key with no
// value and a key set to {} both parse to nothing, so the parsed configuration
// cannot tell them from an absent key, nor a block mapping from a flow one. That
// includes whether this checkout already has an entry, which decides between
// replacing a line and adding one (see configuredWorktreeRootEntry).
//
// A configuration this guidance cannot describe an edit to never reaches it: a
// config that does not load is refused before the repository is registered (see
// assertCheckoutHoldsNoConfiguredWorktreeRoot), and a document nobody can parse
// is one of those.
//
// Every line printed for an existing block carries that block's own indentation,
// for the same reason: the siblings of a block mapping all sit at one column, so
// an entry line at another one is the same unloadable document, and a line named
// for replacement at another one is not a line the operator's file contains.
func printWorktreeRootGuidance(w io.Writer, p *paths.Paths, workingPath, root string) {
	shape := config.InspectGlobalConfigMapping(p.ConfigFile(), "worktree_roots")
	configuredKey, configuredRoot, configured := configuredWorktreeRootEntry(shape, workingPath)
	if configured && worktrees.Canonical(configuredRoot) == worktrees.Canonical(root) {
		fmt.Fprintf(w, "  %s  %s %s\n", sDim.Render("  runs"), sGreen.Render(root), sDim.Render("(already configured)"))
		return
	}
	fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  runs"), root)
	fmt.Fprintln(w)

	indent := worktreeRootsEntryIndent(shape)
	switch {
	case !shape.Present:
		fmt.Fprintf(w, "  %s\n", sDim.Render("Add this to "+p.ConfigFile()+" so runs are created there:"))
		fmt.Fprintf(w, "  %s\n", sBold.Render("worktree_roots:"))
		fmt.Fprintf(w, "  %s\n", sBold.Render(indent+workingPath+": "+root))
	case !shape.AppendableBlock:
		printWorktreeRootsBlockReplacement(w, p, shape, workingPath, configuredKey, root)
	case configured:
		fmt.Fprintf(w, "  %s\n", sDim.Render("Replace this line in "+p.ConfigFile()+" so runs are created there:"))
		fmt.Fprintf(w, "  %s\n", sBold.Render(indent+configuredKey+": "+configuredRoot))
		fmt.Fprintf(w, "  %s\n", sDim.Render("with:"))
		fmt.Fprintf(w, "  %s\n", sBold.Render(indent+configuredKey+": "+root))
	default:
		fmt.Fprintf(w, "  %s\n", sDim.Render("Add this under the existing worktree_roots: in "+p.ConfigFile()+" so runs are created there:"))
		fmt.Fprintf(w, "  %s\n", sBold.Render(indent+workingPath+": "+root))
	}
}

// worktreeRootsEntryIndent is the indentation an entry line must carry: the one
// the block's existing entries use, or two spaces when there are none to match.
func worktreeRootsEntryIndent(shape config.GlobalConfigMapping) string {
	if shape.EntryIndent > 0 {
		return strings.Repeat(" ", shape.EntryIndent)
	}
	return "  "
}

// printWorktreeRootsBlockReplacement prints the block form of the whole
// worktree_roots key, which is the only edit that works when nothing can be
// added under the key as written. It carries every entry the operator already
// has, either re-pointing this checkout's or adding one for it - which is why it
// is reached only for a document that parsed: entries nobody could read are
// entries this rewrite would drop.
func printWorktreeRootsBlockReplacement(w io.Writer, p *paths.Paths, shape config.GlobalConfigMapping, workingPath, configuredKey, root string) {
	if shape.Line != "" {
		fmt.Fprintf(w, "  %s\n", sDim.Render("Replace this line in "+p.ConfigFile()+" so runs are created there:"))
		fmt.Fprintf(w, "  %s\n", sBold.Render(shape.Line))
		fmt.Fprintf(w, "  %s\n", sDim.Render("with:"))
	} else {
		fmt.Fprintf(w, "  %s\n", sDim.Render("Replace the worktree_roots: entry in "+p.ConfigFile()+" so runs are created there:"))
	}
	fmt.Fprintf(w, "  %s\n", sBold.Render("worktree_roots:"))
	repointed := false
	for _, existing := range shape.Entries {
		value := existing.Value
		if configuredKey != "" && existing.Key == configuredKey {
			value = root
			repointed = true
		}
		fmt.Fprintf(w, "  %s\n", sBold.Render("  "+existing.Key+": "+value))
	}
	if !repointed {
		fmt.Fprintf(w, "  %s\n", sBold.Render("  "+workingPath+": "+root))
	}
}

// configuredWorktreeRootEntry returns the worktree_roots entry that places this
// checkout's runs, spelled the way the config file spells it.
//
// It reads the document rather than the parsed configuration for the same reason
// the rest of this guidance does: the document is what the operator edits, and it
// is the same read that already decided the shape, so one source answers every
// question about the file rather than two that can disagree about it.
//
// The key is returned as written rather than canonicalized because guidance that
// names a line has to name the line the operator will find: a trailing separator
// or a symlinked spelling still matches the same checkout (that is what
// worktrees.Canonical is for), and re-writing the key would also risk two
// literal keys naming one checkout, which the config loader rejects.
func configuredWorktreeRootEntry(shape config.GlobalConfigMapping, workingPath string) (key, root string, ok bool) {
	want := worktrees.Canonical(workingPath)
	for _, entry := range shape.Entries {
		if strings.TrimSpace(entry.Key) == "" {
			continue
		}
		if worktrees.Canonical(entry.Key) == want {
			return entry.Key, entry.Value, true
		}
	}
	return "", "", false
}
