package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/integration"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func TestBaseRemoteSubmissionReconciliationUsesSelectedDestination(t *testing.T) {
	for _, requested := range []string{"", "release/customer"} {
		name := "selected default"
		if requested != "" {
			name = "selected override"
		}
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			cliGit(t, work, "init", "-b", "main")
			cliGit(t, work, "config", "user.name", "fixture")
			cliGit(t, work, "config", "user.email", "fixture@localhost")
			cliGit(t, work, "commit", "--allow-empty", "-m", "origin sentinel")
			origin := t.TempDir()
			cliGit(t, origin, "init", "--bare")
			cliGit(t, work, "remote", "add", "origin", origin)
			cliGit(t, work, "push", "origin", "main")
			custom := t.TempDir()
			cliGit(t, custom, "init", "--bare")
			cliGit(t, custom, "symbolic-ref", "HEAD", "refs/heads/customer-main")
			cliGit(t, work, "remote", "add", "custom", custom)
			cliGit(t, work, "checkout", "-b", "customer-main")
			cliGit(t, work, "commit", "--allow-empty", "-m", "customer sentinel")
			cliGit(t, work, "push", "custom", "HEAD:refs/heads/customer-main", "HEAD:refs/heads/release/customer")
			cliGit(t, work, "checkout", "-b", "topic", "main")
			head := cliGit(t, work, "rev-parse", "HEAD")
			originConfig := cliGit(t, work, "config", "--get-regexp", "remote.origin.")
			originRefs := cliGit(t, work, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin/")
			t.Chdir(work)
			ctx := integration.WithRemote(context.Background(), "custom")
			got := gate.SubmissionUpstream(ctx, ".", "main", head, requested, 0)
			wantBase := requested
			if wantBase == "" {
				wantBase = "customer-main"
			}
			if got.URL != custom || got.BaseBranch != wantBase {
				t.Fatalf("reconciliation destination = %+v, want URL %q base %q", got, custom, wantBase)
			}
			if got := cliGit(t, work, "config", "--get-regexp", "remote.origin."); got != originConfig {
				t.Fatalf("reconciliation changed origin config: %q", got)
			}
			if got := cliGit(t, work, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/origin/"); got != originRefs {
				t.Fatalf("reconciliation changed origin refs: %q", got)
			}
		})
	}
}

func TestBaseRemotePushOptionsRefuseAmbiguity(t *testing.T) {
	for _, options := range [][]string{
		{"no-mistakes.base-remote="},
		{"no-mistakes.base-remote=--upload-pack=evil"},
		{"no-mistakes.base-remote=../custom"},
		{"no-mistakes.base-remote=origin", "no-mistakes.base-remote=custom"},
	} {
		if _, err := parseBaseRemotePushOptions(options); err == nil {
			t.Fatalf("accepted ambiguous or invalid selector %q", options)
		}
	}
	got, err := parseBaseRemotePushOptions([]string{"no-mistakes.base-remote=custom"})
	if err != nil || got != "custom" {
		t.Fatalf("selector = %q, %v", got, err)
	}
}

func TestBaseRemotePreflightRefusesMissingRemoteEvenWithoutBaseBranch(t *testing.T) {
	dir := t.TempDir()
	git, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(git, "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("init fixture: %v: %s", err, out)
	}
	t.Chdir(dir)
	ctx := integration.WithRemote(context.Background(), "missing-integration-remote")
	if err := validateAxiRunBaseBranch(ctx, ""); err == nil {
		t.Fatal("missing explicit remote must refuse even without a base override")
	}
}

func TestBaseRemoteRelativeURLFromSubdirectory(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(parent, "checkout")
	customer := filepath.Join(parent, "customer.git")
	cliGit(t, parent, "init", "-b", "main", work)
	cliGit(t, work, "config", "user.name", "fixture")
	cliGit(t, work, "config", "user.email", "fixture@localhost")
	cliGit(t, work, "commit", "--allow-empty", "-m", "customer sentinel")
	cliGit(t, parent, "init", "--bare", customer)
	cliGit(t, customer, "symbolic-ref", "HEAD", "refs/heads/main")
	cliGit(t, work, "remote", "add", "custom", "../customer.git")
	cliGit(t, work, "push", "custom", "HEAD:refs/heads/main", "HEAD:refs/heads/release/customer")
	subdir := filepath.Join(work, "nested", "launch")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	ctx := integration.WithRemote(context.Background(), "custom")
	pin, err := integration.ResolveRequest(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if pin.URL != customer || pin.TransportURL != customer {
		t.Fatalf("root destination = %+v, want local repository %q", pin, customer)
	}
	for _, launch := range []string{work, subdir} {
		t.Run(filepath.Base(launch), func(t *testing.T) {
			t.Chdir(launch)
			for _, base := range []string{"", "release/customer"} {
				if err := validateAxiRunBaseBranch(ctx, base); err != nil {
					t.Fatalf("relative remote preflight for base %q: %v", base, err)
				}
			}
			// Both URL representations must name the sibling bare repository,
			// regardless of the launch directory.
			got, err := integration.ResolveRequest(ctx, ".")
			if err != nil || got.URL != customer || got.TransportURL != customer {
				t.Fatalf("relative destination = %+v, %v; want %q", got, err, customer)
			}
			if remote, err := pin.RuntimeURL(ctx, "."); err != nil || remote != pin.TransportURL {
				t.Fatalf("runtime URL = %q, %v; want %q", remote, err, pin.TransportURL)
			}
			if _, err := integration.ResolveRequest(integration.WithPinned(ctx, pin), "."); err != nil {
				t.Fatalf("pinned continuation: %v", err)
			}
		})
	}
}
