package cli

import (
	"context"
	"os/exec"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/integration"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

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
