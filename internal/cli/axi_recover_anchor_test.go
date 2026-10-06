package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #1233 at the surface an operator actually hits: `no-mistakes axi run`
// refuses to reconcile a private mirror holding private-only commits, and the
// sanctioned recovery path that declares that chain preserved writes an anchor
// reconciliation never consulted - so every recover -> rerun -> push iteration
// re-refused against the evidence the previous iteration had just written.
// With the anchor in place the same command reconciles (archiving the exact
// private head first), lands the submission on the mirror, and the run starts.
func TestTriggerRunReconcilesPrivateMirrorPreservedByRecoveryAnchor(t *testing.T) {
	dir := t.TempDir()
	p := paths.WithRoot(makeSocketSafeTempDir(t))
	t.Setenv("NM_HOME", p.Root())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cliGit(t, dir, "init", "-b", "main")
	cliGit(t, dir, "config", "user.name", "Test")
	cliGit(t, dir, "config", "user.email", "test@example.com")
	cliGit(t, dir, "commit", "--allow-empty", "-m", "base")
	base := cliGit(t, dir, "rev-parse", "HEAD")
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		cliGit(t, dir, "add", name)
		cliGit(t, dir, "commit", "-m", name)
	}
	// The private-only chain: work that never reached the live head, so it is
	// neither contained nor patch-proven nor run-owned.
	write("feature.txt", "private feature\n")
	privateHead := cliGit(t, dir, "rev-parse", "HEAD")
	repo, err := d.InsertRepo(dir, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, dir, "clone", "--bare", dir, gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)
	// The live head diverges onto unrelated work.
	cliGit(t, dir, "reset", "--hard", base)
	write("advanced.txt", "live work\n")
	liveHead := cliGit(t, dir, "rev-parse", "HEAD")

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunsResult{}, nil
	})
	// The daemon registers a run for the submission; triggerRun attaches to it.
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{
			ID:      "01M3TRIGGEREDRUN00000000000",
			RepoID:  repo.ID,
			Branch:  "main",
			HeadSHA: liveHead,
			Status:  types.RunRunning,
		}}, nil
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() { srv.Close(); <-done })
	var client *ipc.Client
	deadline := time.Now().Add(3 * time.Second)
	for {
		client, err = ipc.Dial(p.Socket())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer client.Close()
	chdir(t, dir)
	env := &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Loop iteration 1, no anchor yet: the exact refusal from issue #1233.
	// Nothing may move - the private branch stays put with its work intact.
	_, firstErr := triggerRun(ctx, env, "main", nil, "", "", "", false, "", nil)
	if firstErr == nil {
		t.Fatal("unanchored private-only chain was reconciled instead of refused")
	}
	for _, want := range []string{
		"refusing to reconcile private mirror ref refs/heads/main",
		"at-risk commit(s)",
		privateHead,
	} {
		if !strings.Contains(firstErr.Error(), want) {
			t.Fatalf("refusal did not name %q: %v", want, firstErr)
		}
	}
	if got := cliGit(t, gateDir, "rev-parse", "refs/heads/main"); got != privateHead {
		t.Fatalf("refusal moved the mirror to %s, want %s", got, privateHead)
	}
	t.Logf("attempt 1 refused (issue #1233 deadlock reproduced): %v", firstErr)

	// The sanctioned recovery path declares that exact chain preserved, the
	// way terminalization / `sync --recover` writes it.
	runID := "01M3GQ106JNJRM9DFQSPF48NSE"
	if err := custody.PreserveRecoveryAnchor(ctx, gateDir, custody.RecoveryRef(runID), privateHead); err != nil {
		t.Fatalf("write recovery anchor: %v", err)
	}

	// Loop iteration 2: the same command now reconciles, archives the exact
	// private head before deleting the ref, lands the submission, and the run
	// starts - the loop terminates.
	gotRun, err := triggerRun(ctx, env, "main", nil, "", "", "", false, "", nil)
	if err != nil {
		t.Fatalf("recovery-anchored chain still refused: %v", err)
	}
	if gotRun == "" {
		t.Fatal("anchored submission reconciled but started no run")
	}
	if got := cliGit(t, gateDir, "rev-parse", "refs/heads/main"); got != liveHead {
		t.Fatalf("mirror = %s after submission, want live head %s", got, liveHead)
	}
	archive := "refs/tags/no-mistakes-abandoned/main/" + privateHead
	if got := cliGit(t, gateDir, "rev-parse", archive); got != privateHead {
		t.Fatalf("archive %s = %s, want the exact private head %s", archive, got, privateHead)
	}
	if got := cliGit(t, gateDir, "rev-parse", custody.RecoveryRef(runID)); got != privateHead {
		t.Fatalf("recovery anchor moved to %s, want %s", got, privateHead)
	}
	cliGit(t, gateDir, "merge-base", "--is-ancestor", privateHead, archive)

	// The refusal an operator sees must name every satisfier this fresh-
	// submission call shape can actually take - and no unreachable one.
	for _, want := range []string{
		"refs/no-mistakes/recover/<run>",
		"ancestry",
		"patch-ID",
	} {
		if !strings.Contains(firstErr.Error(), want) {
			t.Fatalf("refusal did not name satisfier %q: %v", want, firstErr)
		}
	}
	if strings.Contains(firstErr.Error(), "Decision 41-A") {
		t.Fatalf("fresh submission offered the unreachable Decision 41-A satisfier: %v", firstErr)
	}
	t.Logf("attempt 2: run %s started; mirror advanced to %s; private head archived at %s; anchor %s intact",
		gotRun, liveHead, archive, custody.RecoveryRef(runID))
	t.Logf("attempt 1 refusal names its satisfiers: %v", firstErr)
}
