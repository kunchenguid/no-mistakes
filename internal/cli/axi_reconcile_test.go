package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type cancelledRunRebindFixture struct {
	t            *testing.T
	p            *paths.Paths
	d            *db.DB
	repo         *db.Repo
	env          *axiEnv
	workDir      string
	gateDir      string
	submitted    string
	cancelledFix string
	current      string
}

func newCancelledRunRebindFixture(t *testing.T, recoverCustody bool) *cancelledRunRebindFixture {
	t.Helper()
	p := paths.WithRoot(makeSocketSafeTempDir(t))
	t.Setenv("NM_HOME", p.Root())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	workDir := t.TempDir()
	cliGit(t, workDir, "init", "-b", "main")
	cliGit(t, workDir, "config", "user.name", "Test")
	cliGit(t, workDir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(workDir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, workDir, "add", "base.txt")
	cliGit(t, workDir, "commit", "-m", "base")
	base := cliGit(t, workDir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workDir, "feature.txt"), []byte("submitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, workDir, "add", "feature.txt")
	cliGit(t, workDir, "commit", "-m", "submitted work")
	submitted := cliGit(t, workDir, "rev-parse", "HEAD")

	repo, err := d.InsertRepo(workDir, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, workDir, "clone", "--bare", workDir, gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, workDir, "remote", "add", gate.RemoteName, gateDir)

	if err := os.WriteFile(filepath.Join(workDir, "cancelled-fix.txt"), []byte("unpublished auto-fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, workDir, "add", "cancelled-fix.txt")
	cliGit(t, workDir, "commit", "-m", "no-mistakes(review): cancelled fix")
	cancelledFix := cliGit(t, workDir, "rev-parse", "HEAD")
	cliGit(t, workDir, "push", gate.RemoteName, cancelledFix+":refs/heads/main")
	cliGit(t, workDir, "reset", "--hard", submitted)

	run, err := d.InsertRun(repo.ID, "main", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, cancelledFix); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, cancelledFix); err != nil {
		t.Fatal(err)
	}
	service := &branchsync.Service{DB: d, Repo: repo, WorkDir: workDir, GateDir: gateDir}
	if recoverCustody {
		recovered := service.Recover(context.Background(), false)
		if !recovered.Recovered || recovered.State != branchsync.StateCustodyReturned {
			t.Fatalf("recover cancelled run = %#v", recovered)
		}
	}

	// The operator restores a corrected version after custody was returned. It
	// intentionally diverges from the cancelled run's unpublished auto-fix.
	cliGit(t, workDir, "reset", "--hard", submitted)
	if err := os.WriteFile(filepath.Join(workDir, "corrected.txt"), []byte("safe corrected version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, workDir, "add", "corrected.txt")
	cliGit(t, workDir, "commit", "-m", "restore corrected version")
	current := cliGit(t, workDir, "rev-parse", "HEAD")

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodClaimLaunchReceipt, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.ClaimLaunchReceiptResult{Receipt: &ipc.LaunchReceipt{
			RunID: "fresh-run", Disposition: "created", LaunchNonce: "nonce", ValidationGeneration: "generation",
			Branch: "main", HeadSHA: current, SubmittedHeadSHA: current, IntentDigest: digestLaunchIntent("validate corrected version"),
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
	t.Cleanup(func() { client.Close() })
	chdir(t, workDir)

	return &cancelledRunRebindFixture{
		t: t, p: p, d: d, repo: repo,
		env:     &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client},
		workDir: workDir, gateDir: gateDir, submitted: submitted, cancelledFix: cancelledFix, current: current,
	}
}

func TestTriggerProofRunRebindsGateAfterCancelledRunCustodyReturned(t *testing.T) {
	f := newCancelledRunRebindFixture(t, true)
	state := inspectAxiBranchSync(context.Background(), f.env)
	if state.State != branchsync.StateCustodyReturned || state.Safety != "custody_returned" || state.Relation != branchsync.RelationDiverged || state.NextAction == nil || state.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-recovery state = %#v", state)
	}

	receipt, err := triggerProofRun(context.Background(), f.env, "main", f.current, nil, "validate corrected version", "", "nonce", "generation")
	if err != nil {
		t.Fatalf("fresh run promised by branch-sync status was refused: %v", err)
	}
	if receipt == nil || receipt.RunID != "fresh-run" {
		t.Fatalf("receipt = %#v", receipt)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/heads/main"); got != f.current {
		t.Fatalf("gate head = %s, want current corrected head %s", got, f.current)
	}
	archive := "refs/tags/no-mistakes-abandoned/main/" + f.cancelledFix
	if got := cliGit(t, f.gateDir, "rev-parse", archive+"^{commit}"); got != f.cancelledFix {
		t.Fatalf("cancelled auto-fix archive = %s, want %s", got, f.cancelledFix)
	}
}

func TestTriggerProofRunStillRefusesUnrecoveredCancelledDivergence(t *testing.T) {
	f := newCancelledRunRebindFixture(t, false)
	gateBefore := cliGit(t, f.gateDir, "rev-parse", "refs/heads/main")

	receipt, err := triggerProofRun(context.Background(), f.env, "main", f.current, nil, "validate corrected version", "", "nonce", "generation")
	if err == nil || receipt != nil {
		t.Fatalf("unsafe fresh run = receipt %#v, err %v", receipt, err)
	}
	var ownershipErr *branchOwnershipError
	if !errors.As(err, &ownershipErr) || ownershipErr.state.State != branchsync.StatePipelineOwned {
		t.Fatalf("unsafe divergence error = %T %v", err, err)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/heads/main"); got != gateBefore {
		t.Fatalf("unsafe refusal moved gate from %s to %s", gateBefore, got)
	}
	if tags := cliGit(t, f.gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("unsafe refusal archived a gate head: %q", tags)
	}
}

func TestTriggerProofRunStillRefusesGateHeadMovedAfterCustodyReturn(t *testing.T) {
	f := newCancelledRunRebindFixture(t, true)
	writer := t.TempDir()
	cliGit(t, writer, "clone", f.gateDir, ".")
	cliGit(t, writer, "config", "user.name", "Test")
	cliGit(t, writer, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(writer, "unrelated-private.txt"), []byte("must not be discarded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, writer, "add", "unrelated-private.txt")
	cliGit(t, writer, "commit", "-m", "independent private gate work")
	unsafeGateHead := cliGit(t, writer, "rev-parse", "HEAD")
	cliGit(t, writer, "push", "origin", "HEAD:refs/heads/main")

	receipt, err := triggerProofRun(context.Background(), f.env, "main", f.current, nil, "validate corrected version", "", "nonce", "generation")
	if err == nil || receipt != nil || !strings.Contains(err.Error(), "at-risk commit") {
		t.Fatalf("moved unsafe gate head = receipt %#v, err %v", receipt, err)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/heads/main"); got != unsafeGateHead {
		t.Fatalf("unsafe refusal moved gate from %s to %s", unsafeGateHead, got)
	}
	if tags := cliGit(t, f.gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("unsafe refusal archived a gate head: %q", tags)
	}
}

func TestTriggerRunRejectedPushRestoresReconciledGateRef(t *testing.T) {
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
	write("feature.txt", "feature\n")
	privateHead := cliGit(t, dir, "rev-parse", "HEAD")
	repo, err := d.InsertRepo(dir, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, dir, "clone", "--bare", dir, gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)
	cliGit(t, dir, "reset", "--hard", base)
	write("advanced.txt", "advanced\n")
	write("feature.txt", "feature\n")
	liveHead := cliGit(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(gateDir, "hooks", "pre-receive"), []byte("#!/bin/sh\necho submission-rejected >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunsResult{}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{}, nil
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runID, err := triggerRun(ctx, env, "main", nil, "", "")
	if err == nil || !strings.Contains(err.Error(), "submission-rejected") || runID != "" {
		t.Fatalf("rejected submission: run=%q err=%v", runID, err)
	}
	if got := cliGit(t, gateDir, "rev-parse", "refs/heads/main"); got != privateHead {
		t.Fatalf("failed submission left mirror at %s, want restored %s", got, privateHead)
	}
	if got := cliGit(t, gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/main/"+privateHead); got != privateHead {
		t.Fatalf("archive = %s, want %s", got, privateHead)
	}
	if got := cliGit(t, dir, "rev-parse", "HEAD"); got != liveHead {
		t.Fatalf("failed submission moved caller head to %s, want %s", got, liveHead)
	}

	receipt, err := triggerProofRun(ctx, env, "main", liveHead, nil, "retry safely", "", "nonce", "generation")
	if err == nil || !strings.Contains(err.Error(), "submission-rejected") || receipt != nil {
		t.Fatalf("rejected proof submission: receipt=%#v err=%v", receipt, err)
	}
	if got := cliGit(t, gateDir, "rev-parse", "refs/heads/main"); got != privateHead {
		t.Fatalf("failed proof submission left mirror at %s, want restored %s", got, privateHead)
	}
}
