package gate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Issue #1555: two no-mistakes homes (two NM_HOMEs, typically two OS
// accounts) share one .git through a linked worktree. Both derive the same
// repository id from the main checkout, so the shared .git/config holds one
// remote.no-mistakes.url, the first home's. init must never rewrite it from
// the second home, on a fresh init or on a refresh.

// sharedHome is one no-mistakes home gating a checkout whose .git it shares.
type sharedHome struct {
	p   *paths.Paths
	d   *db.DB
	dir string // the checkout this home runs init from
	// gitconfig is this home's own global git config, standing in for the
	// account's ~/.gitconfig.
	gitconfig string
}

// newSharedHome creates a home under a fresh temp dir; a non-empty homeLeaf
// names the home's root directory inside it.
func newSharedHome(t *testing.T, dir, homeLeaf string) *sharedHome {
	t.Helper()
	p := paths.WithRoot(filepath.Join(resolveSymlinks(t, t.TempDir()), homeLeaf))
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	gitconfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(gitconfig, nil, 0o644); err != nil {
		t.Fatalf("write global git config: %v", err)
	}
	return &sharedHome{p: p, d: openTestDB(t, p), dir: dir, gitconfig: gitconfig}
}

// act makes git calls in this test run under this home's own global git
// config, as they would in this account's shell.
func (h *sharedHome) act(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", h.gitconfig)
}

// init runs Init as this home: with its NM_HOME paths, its database, and its
// own global git config.
func (h *sharedHome) init(t *testing.T) (*db.Repo, bool, error) {
	t.Helper()
	h.act(t)
	return Init(context.Background(), h.d, h.p, h.dir)
}

// mapGate adds the per-account rewrite the refusal recommends to this home's
// global git config.
func (h *sharedHome) mapGate(t *testing.T, key, value string) {
	t.Helper()
	if out, err := exec.Command("git", "config", "--file", h.gitconfig, key, value).CombinedOutput(); err != nil {
		t.Fatalf("configure %s: %v: %s", key, err, out)
	}
}

// followRecipe runs the command a refusal printed through a POSIX shell, as
// the account would by pasting it, and checks that it mapped otherGate onto
// ownGate in this home's global git config.
func (h *sharedHome) followRecipe(t *testing.T, recipe, ownGate, otherGate string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", recipe)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+h.gitconfig)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run the printed recipe %s: %v: %s", recipe, err, out)
	}
	out, err := exec.Command("git", "config", "--file", h.gitconfig, "--get-all", "url."+ownGate+".insteadOf").Output()
	if err != nil {
		t.Fatalf("the printed recipe did not map onto %s: %v", ownGate, err)
	}
	if got := strings.TrimSuffix(string(out), "\n"); got != otherGate {
		t.Fatalf("the printed recipe mapped %q onto %s, want %q", got, ownGate, otherGate)
	}
}

func (h *sharedHome) unmapGate(t *testing.T, key string) {
	t.Helper()
	if out, err := exec.Command("git", "config", "--file", h.gitconfig, "--unset", key).CombinedOutput(); err != nil {
		t.Fatalf("unset %s: %v: %s", key, err, out)
	}
}

// setupSharedGit returns home A, which has initialized the main checkout, and
// home B, which works in a linked worktree of that same checkout. A non-empty
// homeLeaf names each home's root directory.
func setupSharedGit(t *testing.T, homeLeaf string) (a, b *sharedHome) {
	t.Helper()
	workDir := setupTestRepo(t)
	linked := filepath.Join(resolveSymlinks(t, t.TempDir()), "linked")
	if out, err := exec.Command("git", "-C", workDir, "worktree", "add", "-q", "-b", "b-work", linked).CombinedOutput(); err != nil {
		t.Fatalf("add linked worktree: %v: %s", err, out)
	}
	a = newSharedHome(t, workDir, homeLeaf)
	b = newSharedHome(t, linked, homeLeaf)
	if _, created, err := a.init(t); err != nil || !created {
		t.Fatalf("home A init: created %v, err %v", created, err)
	}
	return a, b
}

// literalGateURL is remote.no-mistakes.url exactly as the shared .git/config
// holds it, before any insteadOf rewrite.
func literalGateURL(t *testing.T, dir string) string {
	t.Helper()
	url, err := gitpkg.GetConfiguredRemoteURL(context.Background(), dir, RemoteName)
	if err != nil {
		t.Fatalf("read literal %s url: %v", RemoteName, err)
	}
	return url
}

func gateBareDir(t *testing.T, h *sharedHome) string {
	t.Helper()
	root, err := gitpkg.FindMainRepoRoot(h.dir)
	if err != nil {
		t.Fatalf("find main repo root: %v", err)
	}
	return h.p.RepoDir(repoID(root))
}

const recipePrefix = "  git config --global "

// assertSharedHomeRefusal checks that err refuses with the guidance the issue
// asks for and returns the insteadOf recipe command it prints.
func assertSharedHomeRefusal(t *testing.T, err error, otherGate, ownGate string) (recipe string) {
	t.Helper()
	if err == nil {
		t.Fatal("init must refuse a gate remote that belongs to another no-mistakes home")
	}
	msg := err.Error()
	for _, want := range []string{
		`remote "no-mistakes" already exists with url ` + strconv.Quote(otherGate),
		"this .git is shared with another NM_HOME",
		"this home's gate is " + ownGate,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal is missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "git remote remove") {
		t.Errorf("refusal must not suggest removing the other home's remote:\n%s", msg)
	}
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, recipePrefix) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("refusal carries no `git config --global` insteadOf recipe:\n%s", msg)
	return ""
}

// The silent overwrite the issue reproduced: B maps A's gate onto its own with
// a url.insteadOf rewrite, and its refresh then wrote B's path into the shared
// config, so A's next `git push no-mistakes` reached B's gate.
func TestInitRefreshLeavesAnotherHomesGateRemoteAloneUnderInsteadOf(t *testing.T) {
	a, b := setupSharedGit(t, "")
	aGate := literalGateURL(t, a.dir)
	bGate := gateBareDir(t, b)
	if aGate == bGate {
		t.Fatalf("homes must have distinct gates, both are %s", aGate)
	}
	b.mapGate(t, "url."+bGate+".insteadOf", aGate)

	if _, created, err := b.init(t); err != nil || !created {
		t.Fatalf("home B fresh init under the rewrite: created %v, err %v", created, err)
	}
	if got := literalGateURL(t, b.dir); got != aGate {
		t.Fatalf("fresh init rewrote the shared remote to %q, want %q", got, aGate)
	}

	repo, created, err := b.init(t)
	if err != nil {
		t.Fatalf("home B refresh under the rewrite: %v", err)
	}
	if created {
		t.Fatal("home B's second init must be a refresh")
	}
	if got := literalGateURL(t, b.dir); got != aGate {
		t.Fatalf("refresh rewrote the shared remote to %q, want %q", got, aGate)
	}
	b.act(t)
	if got, err := gitpkg.GetRemoteURL(context.Background(), b.dir, RemoteName); err != nil || got != b.p.RepoDir(repo.ID) {
		t.Fatalf("home B pushes to %q (err %v), want its own gate %q", got, err, b.p.RepoDir(repo.ID))
	}

	// A, without the rewrite, still reaches its own gate and refreshes cleanly.
	if _, created, err := a.init(t); err != nil || created {
		t.Fatalf("home A refresh: created %v, err %v", created, err)
	}
	a.act(t)
	if got, err := gitpkg.GetRemoteURL(context.Background(), a.dir, RemoteName); err != nil || got != aGate {
		t.Fatalf("home A pushes to %q (err %v), want %q", got, err, aGate)
	}
}

func TestInitRefusesAnotherHomesGateRemoteWithGuidance(t *testing.T) {
	t.Run("fresh init", func(t *testing.T) {
		a, b := setupSharedGit(t, "")
		aGate := literalGateURL(t, a.dir)
		bGate := gateBareDir(t, b)

		_, _, err := b.init(t)
		recipe := assertSharedHomeRefusal(t, err, aGate, bGate)
		if got := literalGateURL(t, b.dir); got != aGate {
			t.Fatalf("refused init changed the shared remote to %q", got)
		}
		if repo, err := b.d.GetRepoByPath(a.dir); err != nil || repo != nil {
			t.Fatalf("refused fresh init must not register the repo in home B: %+v, %v", repo, err)
		}
		if fileExists(bGate) {
			t.Errorf("refused fresh init left its gate behind at %s", bGate)
		}

		// The recipe the refusal prints is what lets B proceed.
		b.followRecipe(t, recipe, bGate, aGate)
		if _, created, err := b.init(t); err != nil || !created {
			t.Fatalf("init after following the recipe: created %v, err %v", created, err)
		}
		if got := literalGateURL(t, b.dir); got != aGate {
			t.Fatalf("init after the recipe changed the shared remote to %q", got)
		}
	})

	t.Run("refresh", func(t *testing.T) {
		a, b := setupSharedGit(t, "")
		aGate := literalGateURL(t, a.dir)
		bGate := gateBareDir(t, b)
		key := "url." + bGate + ".insteadOf"
		b.mapGate(t, key, aGate)
		if _, created, err := b.init(t); err != nil || !created {
			t.Fatalf("home B fresh init under the rewrite: created %v, err %v", created, err)
		}
		b.unmapGate(t, key)

		_, _, err := b.init(t)
		assertSharedHomeRefusal(t, err, aGate, bGate)
		if got := literalGateURL(t, b.dir); got != aGate {
			t.Fatalf("refused refresh changed the shared remote to %q", got)
		}
		if repo, err := b.d.GetRepoByPath(a.dir); err != nil || repo == nil {
			t.Fatalf("a refused refresh must keep home B's registration: %+v, %v", repo, err)
		}
		if !fileExists(bGate) {
			t.Errorf("a refused refresh must keep home B's gate at %s", bGate)
		}
	})
}

// The recipe is pasted into a shell, so gate paths holding shell syntax must
// reach git byte for byte: an unquoted $1 would expand, a stray quote would
// end the word, and git would store a rewrite that never matches.
func TestInitRefusalRecipeSurvivesShellSyntaxInGatePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file names cannot contain a double quote")
	}
	a, b := setupSharedGit(t, `team$1 "nm" it's $(id)`)
	aGate := literalGateURL(t, a.dir)
	bGate := gateBareDir(t, b)

	_, _, err := b.init(t)
	recipe := assertSharedHomeRefusal(t, err, aGate, bGate)
	b.followRecipe(t, recipe, bGate, aGate)

	if _, created, err := b.init(t); err != nil || !created {
		t.Fatalf("init after running the printed recipe: created %v, err %v", created, err)
	}
	if got := literalGateURL(t, b.dir); got != aGate {
		t.Fatalf("init after the recipe changed the shared remote to %q, want %q", got, aGate)
	}
}

// A refresh keeps the promise a fresh init already made: a no-mistakes remote
// the operator pointed somewhere else is theirs, not stale gate wiring.
func TestInitRefreshDoesNotOverwriteACustomNoMistakesRemote(t *testing.T) {
	workDir := setupTestRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()
	if _, _, err := Init(ctx, d, p, workDir); err != nil {
		t.Fatalf("first init: %v", err)
	}
	customRemote := filepath.Join(resolveSymlinks(t, t.TempDir()), "custom.git")
	if _, err := gitpkg.Run(ctx, workDir, "remote", "set-url", RemoteName, customRemote); err != nil {
		t.Fatalf("point remote elsewhere: %v", err)
	}

	_, _, err := Init(ctx, d, p, workDir)
	if err == nil || !strings.Contains(err.Error(), `remote "no-mistakes" already exists with url `+strconv.Quote(customRemote)) {
		t.Fatalf("refresh must refuse a custom remote, got %v", err)
	}
	if strings.Contains(err.Error(), "NM_HOME") {
		t.Errorf("a custom remote is not another home's gate, got:\n%v", err)
	}
	if got := literalGateURL(t, workDir); got != customRemote {
		t.Fatalf("refresh rewrote the custom remote to %q", got)
	}
}

// Stale wiring inside this home's own repos dir is still repaired on refresh.
func TestInitRefreshRepointsStaleWiringInOwnReposDir(t *testing.T) {
	workDir := setupTestRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()
	repo, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("first init: %v", err)
	}
	if _, err := gitpkg.Run(ctx, workDir, "remote", "set-url", RemoteName, p.RepoDir("stale")); err != nil {
		t.Fatalf("point remote at a stale gate: %v", err)
	}

	if _, created, err := Init(ctx, d, p, workDir); err != nil || created {
		t.Fatalf("refresh: created %v, err %v", created, err)
	}
	if got := literalGateURL(t, workDir); got != p.RepoDir(repo.ID) {
		t.Fatalf("refresh left the remote at %q, want %q", got, p.RepoDir(repo.ID))
	}
}

// Under a shared .git, B's own wiring is only a rewrite in B's global config;
// the remote itself is A's. Nothing B tears down may take A's remote with it.
func TestTearingDownAnotherHomesGateKeepsTheSharedRemote(t *testing.T) {
	t.Run("fresh init rollback", func(t *testing.T) {
		a, b := setupSharedGit(t, "")
		aGate := literalGateURL(t, a.dir)
		b.mapGate(t, "url."+gateBareDir(t, b)+".insteadOf", aGate)
		old := ensureGateHooksPathIsolation
		ensureGateHooksPathIsolation = func(context.Context, string) (bool, error) {
			return false, errors.New("injected provisioning failure")
		}
		t.Cleanup(func() { ensureGateHooksPathIsolation = old })

		if _, _, err := b.init(t); err == nil {
			t.Fatal("init must fail when provisioning fails")
		}
		if got := literalGateURL(t, b.dir); got != aGate {
			t.Fatalf("rollback changed the shared remote to %q, want %q", got, aGate)
		}
	})

	t.Run("eject", func(t *testing.T) {
		a, b := setupSharedGit(t, "")
		aGate := literalGateURL(t, a.dir)
		b.mapGate(t, "url."+gateBareDir(t, b)+".insteadOf", aGate)
		if _, created, err := b.init(t); err != nil || !created {
			t.Fatalf("home B init under the rewrite: created %v, err %v", created, err)
		}

		if _, err := Eject(context.Background(), b.d, b.p, b.dir); err != nil {
			t.Fatalf("home B eject: %v", err)
		}
		if got := literalGateURL(t, b.dir); got != aGate {
			t.Fatalf("eject changed the shared remote to %q, want %q", got, aGate)
		}
		if fileExists(gateBareDir(t, b)) {
			t.Error("eject must still remove home B's own gate")
		}
	})
}

// A failed fresh init removes only the remote it wrote. A copied checkout's
// remote to the original's gate sits in the same repos dir, but this init
// never wrote it.
func TestInitRollbackKeepsARemoteItDidNotWrite(t *testing.T) {
	workDir := setupTestRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	siblingGate := p.RepoDir("original")
	if err := gitpkg.AddRemote(context.Background(), workDir, RemoteName, siblingGate); err != nil {
		t.Fatalf("add sibling gate remote: %v", err)
	}
	old := ensureGateHooksPathIsolation
	ensureGateHooksPathIsolation = func(context.Context, string) (bool, error) {
		return false, errors.New("injected provisioning failure")
	}
	t.Cleanup(func() { ensureGateHooksPathIsolation = old })

	if _, _, err := Init(context.Background(), d, p, workDir); err == nil {
		t.Fatal("init must fail when provisioning fails")
	}
	if got := literalGateURL(t, workDir); got != siblingGate {
		t.Fatalf("rollback changed a remote this init never wrote to %q, want %q", got, siblingGate)
	}
}

// A rewrite that maps only fetches onto this gate is not enough: the pipeline
// pushes through the remote, and a pushInsteadOf sending pushes elsewhere would
// leave this home's runs unreachable.
func TestInitRefusesAnotherHomesGateWhenOnlyFetchesReachThisGate(t *testing.T) {
	a, b := setupSharedGit(t, "")
	aGate := literalGateURL(t, a.dir)
	bGate := gateBareDir(t, b)
	b.mapGate(t, "url."+bGate+".insteadOf", aGate)
	b.mapGate(t, "url."+filepath.Join(t.TempDir(), "elsewhere.git")+".pushInsteadOf", aGate)

	_, _, err := b.init(t)
	assertSharedHomeRefusal(t, err, aGate, bGate)
	if got := literalGateURL(t, b.dir); got != aGate {
		t.Fatalf("refused init changed the shared remote to %q", got)
	}
}

// A remote with several URLs pushes to all of them, so init cannot judge it
// from one and leaves it alone.
func TestInitRefusesANoMistakesRemoteWithSeveralURLs(t *testing.T) {
	workDir := setupTestRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()
	repo, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("first init: %v", err)
	}
	extra := filepath.Join(resolveSymlinks(t, t.TempDir()), "extra.git")
	if _, err := gitpkg.Run(ctx, workDir, "remote", "set-url", "--add", RemoteName, extra); err != nil {
		t.Fatalf("add a second url: %v", err)
	}

	_, _, err = Init(ctx, d, p, workDir)
	if err == nil || !strings.Contains(err.Error(), "has 2 urls configured") {
		t.Fatalf("init must refuse a remote with several urls, got %v", err)
	}
	urls, err := gitpkg.GetConfiguredRemoteURLs(ctx, workDir, RemoteName)
	if err != nil || len(urls) != 2 || urls[0] != p.RepoDir(repo.ID) || urls[1] != extra {
		t.Fatalf("remote urls = %q (err %v), want both left in place", urls, err)
	}
}

func TestIsSameRepoGateOfAnotherHome(t *testing.T) {
	root := resolveSymlinks(t, t.TempDir())
	bareDir := filepath.Join(root, "b", ".no-mistakes", "repos", "abc123.git")
	for _, tc := range []struct {
		name string
		url  string
		want bool
	}{
		{"same id under another home", filepath.Join(root, "a", ".no-mistakes", "repos", "abc123.git"), true},
		{"another repository's id", filepath.Join(root, "a", ".no-mistakes", "repos", "def456.git"), false},
		{"not under a repos dir", filepath.Join(root, "a", "custom", "abc123.git"), false},
		{"relative path", filepath.Join("repos", "abc123.git"), false},
		{"file URL", "file://" + filepath.ToSlash(filepath.Join(root, "a", ".no-mistakes", "repos", "abc123.git")), false},
		{"network URL", "git@example.com:repos/abc123.git", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSameRepoGateOfAnotherHome(tc.url, bareDir); got != tc.want {
				t.Fatalf("isSameRepoGateOfAnotherHome(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}
