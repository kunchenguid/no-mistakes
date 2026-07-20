package steps

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// TestBuildHostGitHubUsesCanonicalURL is the buildHost half of the issue #290
// fix: given the canonical upstream URL (which pr/ci pass after resolving the
// ssh alias via scm.CanonicalRemoteURL), gh must be scoped to the real
// host/repo, never the ssh-config alias — even when sctx.Repo.UpstreamURL still
// holds the alias form.
func TestBuildHostGitHubUsesCanonicalURL(t *testing.T) {
	env, logFile := fakeGH(t, "")
	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: t.TempDir(),
		Repo:    &db.Repo{UpstreamURL: "git@github-acme:test/repo.git"}, // raw ssh alias
		Run:     &db.Run{},
		Env:     env,
		Log:     func(string) {},
	}

	host, skip := buildHost(sctx, scm.ProviderGitHub, "git@github.com:test/repo.git")
	if host == nil {
		t.Fatalf("buildHost returned nil host, skip reason: %q", skip)
	}
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("host.Available() = %v", err)
	}
	if _, err := host.FindPR(context.Background(), "feature", "main"); err != nil {
		t.Fatalf("host.FindPR() = %v", err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logData)
	if !strings.Contains(log, "--hostname github.com") {
		t.Errorf("gh auth check not scoped to canonical host; gh log:\n%s", log)
	}
	if !strings.Contains(log, "--repo test/repo") {
		t.Errorf("gh not scoped to canonical repo slug; gh log:\n%s", log)
	}
	if strings.Contains(log, "github-acme") {
		t.Errorf("gh invoked with the ssh alias instead of the canonical host; gh log:\n%s", log)
	}
}
