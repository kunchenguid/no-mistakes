package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestAxiRunRefusalNamesAdoptPublishedOnlyWhereItApplies pins the help a fresh
// `axi run` attaches to its private-mirror refusal (issue #1293). The refusal
// used to end at proofs a hand-resolved rebase can never supply; it now names
// the supported exit, but only for the refusal that exit clears and only on a
// branch where the command would not refuse in turn.
func TestAxiRunRefusalNamesAdoptPublishedOnlyWhereItApplies(t *testing.T) {
	f := newCLIUnmovedAbortFixture(t)
	rebased := rebaseReleasedBranchOverConflict(t, f, true)
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.GetRepo(f.repoID)
	if err != nil || repo == nil {
		t.Fatalf("load repo: %#v, %v", repo, err)
	}
	env := &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig()}
	ctx := context.Background()

	// The exact refusal triggerRun returns for this submission.
	_, reconcileErr := gate.ReconcileStaleBranch(ctx, f.gate, ".", "feature/recover", rebased, "", nil)
	if reconcileErr == nil || !strings.Contains(reconcileErr.Error(), "refusing to reconcile private mirror ref refs/heads/feature/recover") {
		t.Fatalf("hand-resolved rebase was not refused: %v", reconcileErr)
	}
	refusal := fmt.Errorf("prepare private mirror for %q: %w", "feature/recover", reconcileErr)

	help := adoptPublishedHelp(ctx, env, refusal)
	if len(help) != 1 || !strings.Contains(help[0], "`no-mistakes axi sync --adopt-published`") || !strings.Contains(help[0], "`no-mistakes axi run`") {
		t.Fatalf("refusal help = %q, want the adoption command and the retry", help)
	}
	if got := adoptPublishedHelp(ctx, env, errors.New("push \"feature/recover\" to gate: rejected")); got != nil {
		t.Fatalf("an unrelated failure offered adoption: %q", got)
	}

	// The same refusal on a branch adoption does not cover names nothing.
	cliGit(t, f.local, "checkout", "main")
	if got := adoptPublishedHelp(ctx, env, refusal); got != nil {
		t.Fatalf("refusal on a branch adoption cannot clear offered it: %q", got)
	}
}
