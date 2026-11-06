package cli

import (
	"context"
	"os/exec"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
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
			env := &axiEnv{repo: &db.Repo{DefaultBranch: "main"}}
			got := submissionReconciliationUpstream(ctx, env, head, requested)
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
