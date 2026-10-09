package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// setupForkLayoutRepo builds a working repo whose "origin" is the GitHub fork
// URL originURL and, when upstreamURL is non-empty, an additional "upstream"
// remote pointing at upstreamURL - the exact layout `gh repo fork --clone`
// leaves behind (issue #1178). Both GitHub URLs are redirected via
// url.insteadOf to local bare repos so init's git operations (adding the gate
// origin remote, ls-remote for the default branch) resolve without touching
// the network; only the literal configured URL - what our detection reads -
// stays the GitHub one.
func setupForkLayoutRepo(t *testing.T, originURL, upstreamURL string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))

	work := filepath.Join(resolveSymlinks(t, t.TempDir()), "work")
	if out, err := exec.Command("git", "init", work).CombinedOutput(); err != nil {
		t.Fatalf("init work: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "config", "user.email", "test@test.com").CombinedOutput(); err != nil {
		t.Fatalf("config email: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", work, "config", "user.name", "Test").CombinedOutput(); err != nil {
		t.Fatalf("config name: %v: %s", err, out)
	}

	localOrigin := filepath.Join(resolveSymlinks(t, t.TempDir()), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", localOrigin).CombinedOutput(); err != nil {
		t.Fatalf("init local origin: %v: %s", err, out)
	}
	configureGitInsteadOf(t, work, originURL, localOrigin)
	if out, err := exec.Command("git", "-C", work, "remote", "add", "origin", originURL).CombinedOutput(); err != nil {
		t.Fatalf("add origin: %v: %s", err, out)
	}

	if upstreamURL != "" {
		localUpstream := filepath.Join(resolveSymlinks(t, t.TempDir()), "upstream.git")
		if out, err := exec.Command("git", "init", "--bare", localUpstream).CombinedOutput(); err != nil {
			t.Fatalf("init local upstream: %v: %s", err, out)
		}
		configureGitInsteadOf(t, work, upstreamURL, localUpstream)
		if out, err := exec.Command("git", "-C", work, "remote", "add", "upstream", upstreamURL).CombinedOutput(); err != nil {
			t.Fatalf("add upstream: %v: %s", err, out)
		}
	}

	if out, err := exec.Command("git", "-C", work, "commit", "--allow-empty", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("initial commit: %v: %s", err, out)
	}
	return work
}

func stubResolveForkParent(t *testing.T, fn func(ctx context.Context, remoteURL string) (string, bool, error)) *int {
	t.Helper()
	calls := 0
	old := resolveForkParent
	resolveForkParent = func(ctx context.Context, remoteURL string) (string, bool, error) {
		calls++
		return fn(ctx, remoteURL)
	}
	t.Cleanup(func() { resolveForkParent = old })
	return &calls
}

func TestInitRefusesForkOriginWhenUpstreamNamesTheRealParent(t *testing.T) {
	originURL := "https://github.com/fork-owner/no-mistakes.git"
	upstreamURL := "https://github.com/parent-owner/no-mistakes.git"
	work := setupForkLayoutRepo(t, originURL, upstreamURL)

	calls := stubResolveForkParent(t, func(ctx context.Context, remoteURL string) (string, bool, error) {
		if remoteURL != originURL {
			t.Fatalf("resolveForkParent called with %q, want origin URL %q", remoteURL, originURL)
		}
		return "parent-owner/no-mistakes", true, nil
	})

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	_, _, err := Init(context.Background(), d, p, work)
	if err == nil {
		t.Fatal("expected refusal for fork-as-origin layout")
	}
	msg := err.Error()
	for _, want := range []string{
		"fork-owner/no-mistakes",
		"parent-owner/no-mistakes",
		"git remote set-url origin",
		"--fork-url",
		"CONTRIBUTING.md",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message missing %q; got: %q", want, msg)
		}
	}
	if *calls != 1 {
		t.Fatalf("resolveForkParent called %d times, want 1", *calls)
	}

	// No gate must have been created by the refused init.
	repo, err := d.GetRepoByPath(work)
	if err != nil {
		t.Fatalf("get repo by path: %v", err)
	}
	if repo != nil {
		t.Fatalf("refused init must not register a repo, got %+v", repo)
	}
}

func TestInitProceedsWithoutAnUpstreamRemote(t *testing.T) {
	originURL := "https://github.com/fork-owner/no-mistakes.git"
	work := setupForkLayoutRepo(t, originURL, "")

	calls := stubResolveForkParent(t, func(context.Context, string) (string, bool, error) {
		t.Fatal("resolveForkParent must not be called without an 'upstream' remote")
		return "", false, nil
	})

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	if _, _, err := Init(context.Background(), d, p, work); err != nil {
		t.Fatalf("init without an upstream remote should succeed: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("resolveForkParent called %d times, want 0", *calls)
	}
}

func TestInitProceedsWhenUpstreamNamesAnUnrelatedRepo(t *testing.T) {
	originURL := "https://github.com/fork-owner/no-mistakes.git"
	// Different repo name entirely: a template/reference remote, not a fork
	// parent, must never trigger a gh lookup.
	upstreamURL := "https://github.com/some-owner/other-project.git"
	work := setupForkLayoutRepo(t, originURL, upstreamURL)

	calls := stubResolveForkParent(t, func(context.Context, string) (string, bool, error) {
		t.Fatal("resolveForkParent must not be called for an unrelated 'upstream' remote")
		return "", false, nil
	})

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	if _, _, err := Init(context.Background(), d, p, work); err != nil {
		t.Fatalf("init with an unrelated upstream remote should succeed: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("resolveForkParent called %d times, want 0", *calls)
	}
}

// writeGhHostsConfig writes a synthetic gh hosts.yml naming hosts and points
// GH_CONFIG_DIR at it, so scm.DetectProviderContext recognizes each as GitHub
// Enterprise Server via ghKnowsHost without depending on the real machine's gh
// config. Mirrors internal/scm's own writeGhConfig test helper.
func writeGhHostsConfig(t *testing.T, hosts ...string) {
	t.Helper()
	var body strings.Builder
	for _, host := range hosts {
		body.WriteString(host)
		body.WriteString(":\n  git_protocol: https\n")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(body.String()), 0o644); err != nil {
		t.Fatalf("write hosts.yml: %v", err)
	}
	t.Setenv("GH_CONFIG_DIR", dir)
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestInitProceedsWhenUpstreamSharesOwnerAndNameOnADifferentHost(t *testing.T) {
	// A GitHub fork and its parent always share a host (forking across
	// instances does not exist), so a same-named "parent-owner/no-mistakes"
	// on a DIFFERENT Enterprise Server instance from origin can never
	// actually be origin's parent - even though owner and name read
	// identically. This must be rejected on host alone, without ever calling
	// gh (Greptile finding on PR #1183).
	writeGhHostsConfig(t, "ghe-a.example.com", "ghe-b.example.com")

	originURL := "https://ghe-a.example.com/fork-owner/no-mistakes.git"
	upstreamURL := "https://ghe-b.example.com/parent-owner/no-mistakes.git"
	work := setupForkLayoutRepo(t, originURL, upstreamURL)

	calls := stubResolveForkParent(t, func(context.Context, string) (string, bool, error) {
		t.Fatal("resolveForkParent must not be called when origin and upstream are on different hosts")
		return "", false, nil
	})

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	if _, _, err := Init(context.Background(), d, p, work); err != nil {
		t.Fatalf("init with a cross-host upstream should succeed: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("resolveForkParent called %d times, want 0", *calls)
	}
}

func TestInitProceedsWhenForkParentCannotBeConfirmed(t *testing.T) {
	originURL := "https://github.com/fork-owner/no-mistakes.git"
	upstreamURL := "https://github.com/parent-owner/no-mistakes.git"
	work := setupForkLayoutRepo(t, originURL, upstreamURL)

	// Simulates gh being unavailable, unauthenticated, or offline: detection
	// must fail open, not closed.
	stubResolveForkParent(t, func(context.Context, string) (string, bool, error) {
		return "", false, errFakeGHUnavailable
	})

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	if _, _, err := Init(context.Background(), d, p, work); err != nil {
		t.Fatalf("init must fail open when the parent cannot be confirmed: %v", err)
	}
}

func TestInitProceedsWhenOriginIsNotActuallyAFork(t *testing.T) {
	originURL := "https://github.com/fork-owner/no-mistakes.git"
	upstreamURL := "https://github.com/parent-owner/no-mistakes.git"
	work := setupForkLayoutRepo(t, originURL, upstreamURL)

	stubResolveForkParent(t, func(context.Context, string) (string, bool, error) {
		return "", false, nil
	})

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	if _, _, err := Init(context.Background(), d, p, work); err != nil {
		t.Fatalf("init must succeed when origin is confirmed not a fork: %v", err)
	}
}

func TestInitProceedsWhenGHReportsADifferentParent(t *testing.T) {
	originURL := "https://github.com/fork-owner/no-mistakes.git"
	upstreamURL := "https://github.com/parent-owner/no-mistakes.git"
	work := setupForkLayoutRepo(t, originURL, upstreamURL)

	// origin is a fork, but of some other repo - not the one "upstream" names.
	stubResolveForkParent(t, func(context.Context, string) (string, bool, error) {
		return "someone-else/no-mistakes", true, nil
	})

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	if _, _, err := Init(context.Background(), d, p, work); err != nil {
		t.Fatalf("init must succeed when gh names a different parent than upstream: %v", err)
	}
}

func TestInitWithExplicitForkURLSkipsMisroutingCheck(t *testing.T) {
	// The correct topology per CONTRIBUTING.md: origin IS the parent, and the
	// fork is passed explicitly. An unrelated (or even matching-looking)
	// "upstream" remote must not trigger the new check at all.
	parentURL := "https://github.com/parent-owner/no-mistakes.git"
	forkAsUpstreamURL := "https://github.com/fork-owner/no-mistakes.git"
	work := setupForkLayoutRepo(t, parentURL, forkAsUpstreamURL)

	calls := stubResolveForkParent(t, func(context.Context, string) (string, bool, error) {
		t.Fatal("resolveForkParent must not be called when --fork-url is given explicitly")
		return "", false, nil
	})

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	forkURL := "https://github.com/fork-owner/no-mistakes.git"
	if _, _, err := InitWithFork(context.Background(), d, p, work, forkURL); err != nil {
		t.Fatalf("init --fork-url should succeed: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("resolveForkParent called %d times, want 0", *calls)
	}
}

func TestInitPlainReinitWithRecordedForkSkipsMisroutingCheck(t *testing.T) {
	parentURL := "https://github.com/parent-owner/no-mistakes.git"
	forkURL := "https://github.com/fork-owner/no-mistakes.git"
	work := setupForkLayoutRepo(t, parentURL, "")

	nmRoot := t.TempDir()
	p := paths.WithRoot(nmRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)

	if _, _, err := InitWithFork(context.Background(), d, p, work, forkURL); err != nil {
		t.Fatalf("first init with fork: %v", err)
	}

	// Now the contributor adds an "upstream" remote (harmlessly, or out of
	// habit) that happens to equal origin's already-configured fork target's
	// name. A plain re-init must still skip the check: the fork is already
	// recorded.
	localUpstream := filepath.Join(resolveSymlinks(t, t.TempDir()), "upstream2.git")
	if out, err := exec.Command("git", "init", "--bare", localUpstream).CombinedOutput(); err != nil {
		t.Fatalf("init local upstream: %v: %s", err, out)
	}
	configureGitInsteadOf(t, work, forkURL, localUpstream)
	if out, err := exec.Command("git", "-C", work, "remote", "add", "upstream", forkURL).CombinedOutput(); err != nil {
		t.Fatalf("add upstream: %v: %s", err, out)
	}

	calls := stubResolveForkParent(t, func(context.Context, string) (string, bool, error) {
		t.Fatal("resolveForkParent must not be called when a fork URL is already recorded")
		return "", false, nil
	})

	if _, _, err := Init(context.Background(), d, p, work); err != nil {
		t.Fatalf("plain re-init with a recorded fork should succeed: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("resolveForkParent called %d times, want 0", *calls)
	}
}

var errFakeGHUnavailable = &fakeGHUnavailableError{}

type fakeGHUnavailableError struct{}

func (e *fakeGHUnavailableError) Error() string { return "gh: not authenticated" }
