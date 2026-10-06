package steps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
)

// TestIntegrationRemote_BranchOnlyOnAnotherRemote is the reported failure:
// the base branch exists only on a second remote, while the remote named
// origin is a different project. Checking origin must still say the branch
// does not exist there. The same other remote then has to serve the
// existence check, the fetch (without rewriting refs/remotes/origin/*), the
// push URL, and the GitLab project the merge request is opened against.
func TestIntegrationRemote_BranchOnlyOnAnotherRemote(t *testing.T) {
	ctx := context.Background()
	const (
		release = "console/release_5.3.x_gzyh"
		head    = "hotfix"
	)

	product := t.TempDir()
	customer := t.TempDir()
	gitCmd(t, product, "init", "--bare", "-b", "main")
	gitCmd(t, customer, "init", "--bare", "-b", "main")

	seed := t.TempDir()
	gitCmd(t, seed, "init", "-b", "main")
	gitCmd(t, seed, "remote", "add", "origin", product)
	writeFile(t, seed, "product.txt", "product\n")
	gitCmd(t, seed, "add", "product.txt")
	gitCmd(t, seed, "commit", "-m", "product main")
	gitCmd(t, seed, "push", "origin", "main")
	writeFile(t, seed, "customer.txt", "customer\n")
	gitCmd(t, seed, "add", "customer.txt")
	gitCmd(t, seed, "commit", "-m", "customer release")
	releaseTip := gitCmd(t, seed, "rev-parse", "HEAD")
	gitCmd(t, seed, "remote", "add", "custom", customer)
	gitCmd(t, seed, "push", "custom", "HEAD:refs/heads/"+release)

	user := t.TempDir()
	gitCmd(t, user, "clone", product, ".")
	gitCmd(t, user, "remote", "add", "custom", customer)
	gitCmd(t, user, "fetch", "custom")
	gitCmd(t, user, "checkout", "-b", head, "--track", "custom/"+release)
	if got := gitCmd(t, user, "config", "--get", "branch."+head+".remote"); got != "custom" {
		t.Fatalf("branch.%s.remote = %q, want custom", head, got)
	}
	if got := gitCmd(t, user, "config", "--get", "branch."+head+".merge"); got != "refs/heads/"+release {
		t.Fatalf("branch.%s.merge = %q, want refs/heads/%s", head, got, release)
	}

	originErr := VerifyRemoteBranchExists(ctx, user, "origin", release)
	if originErr == nil || !strings.Contains(originErr.Error(), `remote branch "`+release+`" does not exist on origin`) {
		t.Fatalf("origin check = %v, want remote branch %q does not exist on origin", originErr, release)
	}

	resolved, err := ResolveIntegrationRemote(ctx, user, head, release, "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "custom" {
		t.Fatalf("tracking resolution = %q, want custom", resolved)
	}
	if err := VerifyRemoteBranchExists(ctx, user, resolved, release); err != nil {
		t.Fatalf("custom check: %v", err)
	}

	explicitOrigin, err := ResolveIntegrationRemote(ctx, user, head, release, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if explicitOrigin != "origin" {
		t.Fatalf("explicit origin = %q, want origin", explicitOrigin)
	}
	if err := VerifyRemoteBranchExists(ctx, user, explicitOrigin, release); err == nil || !strings.Contains(err.Error(), "does not exist on origin") {
		t.Fatalf("explicit origin check = %v, want the origin miss", err)
	}

	stored, err := StoredIntegrationRemote(ctx, user, "main", release, "")
	if err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		t.Fatalf("unrelated tracking stored %q, want empty (origin default)", stored)
	}

	gate := t.TempDir()
	gitCmd(t, gate, "clone", product, ".")
	if err := VerifyIntegrationBranch(ctx, gate, user, "custom", release); err != nil {
		t.Fatalf("gate lookup of custom: %v", err)
	}

	customName := "custom"
	sctx := &pipeline.StepContext{
		Ctx:     ctx,
		WorkDir: gate,
		Repo: &db.Repo{
			WorkingPath:   user,
			UpstreamURL:   product,
			DefaultBranch: "main",
		},
		Run: &db.Run{
			Branch:       head,
			PRBaseBranch: strPtr(release),
			BaseRemote:   &customName,
		},
	}
	tip, ok := resolveRunDefaultBranchTip(ctx, sctx, "", release)
	if !ok || tip != releaseTip {
		t.Fatalf("fetched tip = %q ok=%v, want %s", tip, ok, releaseTip)
	}
	if got := gitCmd(t, gate, "rev-parse", "--verify", "refs/remotes/custom/"+release); got != releaseTip {
		t.Fatalf("custom tracking ref = %q, want %s", got, releaseTip)
	}
	if _, err := exec.Command("git", "-C", gate, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+release).Output(); err == nil {
		t.Fatal("fetch wrote refs/remotes/origin/" + release)
	}
	if push := resolvePushURL(sctx); push != customer {
		t.Fatalf("resolvePushURL = %q, want custom remote %q", push, customer)
	}
	sctx.Repo.ForkURL = "https://github.com/example/fork.git"
	if push := resolvePushURL(sctx); push != sctx.Repo.ForkURL {
		t.Fatalf("resolvePushURL with fork = %q, want %q", push, sctx.Repo.ForkURL)
	}

	// Step-time tracking, with nothing persisted, uses the same remote.
	sctx.Run.BaseRemote = nil
	sctx.Repo.ForkURL = ""
	gate2 := t.TempDir()
	gitCmd(t, gate2, "clone", product, ".")
	sctx.WorkDir = gate2
	tip, ok = resolveRunDefaultBranchTip(ctx, sctx, "", release)
	if !ok || tip != releaseTip {
		t.Fatalf("tracking fetch tip = %q ok=%v, want %s", tip, ok, releaseTip)
	}
	if _, err := exec.Command("git", "-C", gate2, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+release).Output(); err == nil {
		t.Fatal("tracking fetch wrote refs/remotes/origin/" + release)
	}
}

func TestIntegrationRemote_GitLabProjectComesFromTheRemoteURL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-b", "main")
	const (
		productURL  = "https://gitlab.example.com/dt-insight-front/dt-insight-studio.git"
		customerURL = "https://gitlab.example.com/customltem/dt-insight-studio.git"
	)
	gitCmd(t, dir, "remote", "add", "origin", productURL)
	gitCmd(t, dir, "remote", "add", "custom", customerURL)

	custom := "custom"
	sctx := &pipeline.StepContext{
		Ctx: ctx,
		Repo: &db.Repo{
			WorkingPath: dir,
			UpstreamURL: productURL,
		},
		Run: &db.Run{
			Branch:     "hotfix",
			BaseRemote: &custom,
		},
	}
	host, skip := buildHost(sctx, scm.ProviderGitLab)
	if skip != "" {
		t.Fatal(skip)
	}
	gl, ok := host.(*gitlab.Host)
	if !ok {
		t.Fatalf("host type %T, want *gitlab.Host", host)
	}
	if gl.Project() != "customltem/dt-insight-studio" {
		t.Fatalf("project = %q, want customltem/dt-insight-studio", gl.Project())
	}

	sctx.Run.BaseRemote = nil
	host, skip = buildHost(sctx, scm.ProviderGitLab)
	if skip != "" {
		t.Fatal(skip)
	}
	gl = host.(*gitlab.Host)
	if gl.Project() != "dt-insight-front/dt-insight-studio" {
		t.Fatalf("origin project = %q, want the registered upstream", gl.Project())
	}
}

func strPtr(s string) *string { return &s }

func writeFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
