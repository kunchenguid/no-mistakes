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

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTriggerProofRunReconcilesReturnedCustodyHeadBeforeRebasedSubmission(t *testing.T) {
	f := newReturnedCustodyReconcileFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	receipt, err := triggerProofRun(ctx, f.env, f.branch, f.liveHead, nil, "preserve monthly coupons", "", "retry-1", "review-retry")
	if err != nil {
		t.Fatalf("fresh run prescribed after custody return was refused: %v", err)
	}
	if receipt == nil || receipt.HeadSHA != f.liveHead || receipt.SubmittedHeadSHA != f.liveHead {
		t.Fatalf("launch receipt = %+v, want head %s", receipt, f.liveHead)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/heads/"+f.branch); got != f.liveHead {
		t.Fatalf("gate branch = %s, want clean rebased submission %s", got, f.liveHead)
	}
	archive := "refs/tags/no-mistakes-abandoned/" + f.branch + "/" + f.returnedHead
	if got := cliGit(t, f.gateDir, "rev-parse", archive); got != f.returnedHead {
		t.Fatalf("stale returned-custody head archive = %s, want %s", got, f.returnedHead)
	}
	for _, anchored := range []struct {
		dir string
		ref string
	}{
		{f.dir, custody.RecoveryRef(f.run.ID)},
		{f.gateDir, custody.RecoveryRef(f.run.ID)},
	} {
		if got := cliGit(t, anchored.dir, "rev-parse", anchored.ref); got != f.returnedHead {
			t.Fatalf("recovery anchor %s in %s = %s, want %s", anchored.ref, anchored.dir, got, f.returnedHead)
		}
	}
	prior, err := f.env.d.GetRun(f.run.ID)
	if err != nil || prior == nil || prior.HeadSHA != f.returnedHead || prior.CustodyReturnedAt == nil {
		t.Fatalf("prior run lost its audit binding: run=%+v err=%v", prior, err)
	}
}

func TestPrepareFreshRunGateBranchRefusesUnsafeReturnedCustodyEvidence(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, *returnedCustodyReconcileFixture) string
		wantErr string
	}{
		{
			name: "dirty worktree",
			mutate: func(t *testing.T, f *returnedCustodyReconcileFixture) string {
				t.Helper()
				if err := os.WriteFile(filepath.Join(f.dir, "uncommitted.txt"), []byte("not submitted\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return f.liveHead
			},
			wantErr: "worktree is not clean",
		},
		{
			name: "submission head changed",
			mutate: func(_ *testing.T, f *returnedCustodyReconcileFixture) string {
				return f.submitted
			},
			wantErr: "branch changed before submission",
		},
		{
			name: "missing worktree anchor",
			mutate: func(t *testing.T, f *returnedCustodyReconcileFixture) string {
				cliGit(t, f.dir, "update-ref", "-d", custody.RecoveryRef(f.run.ID))
				return f.liveHead
			},
			wantErr: "invoking worktree recovery anchor",
		},
		{
			name: "missing gate anchor",
			mutate: func(t *testing.T, f *returnedCustodyReconcileFixture) string {
				cliGit(t, f.gateDir, "update-ref", "-d", custody.RecoveryRef(f.run.ID))
				return f.liveHead
			},
			wantErr: "local gate recovery anchor",
		},
		{
			name: "symbolic gate anchor",
			mutate: func(t *testing.T, f *returnedCustodyReconcileFixture) string {
				anchor := custody.RecoveryRef(f.run.ID)
				cliGit(t, f.gateDir, "update-ref", "-d", anchor)
				cliGit(t, f.gateDir, "symbolic-ref", anchor, "refs/heads/"+f.branch)
				return f.liveHead
			},
			wantErr: "is symbolic",
		},
		{
			name: "unverified terminal head",
			mutate: func(t *testing.T, f *returnedCustodyReconcileFixture) string {
				if err := f.env.d.UpdateRunStatus(f.run.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
				return f.liveHead
			},
			wantErr: "evidence no longer matches",
		},
		{
			name: "unexpected divergent gate head",
			mutate: func(t *testing.T, f *returnedCustodyReconcileFixture) string {
				cliGit(t, f.dir, "switch", "--detach", f.returnedHead)
				if err := os.WriteFile(filepath.Join(f.dir, "unexpected.txt"), []byte("unowned gate work\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				cliGit(t, f.dir, "add", "unexpected.txt")
				cliGit(t, f.dir, "commit", "-m", "unexpected gate work")
				unexpected := cliGit(t, f.dir, "rev-parse", "HEAD")
				cliGit(t, f.dir, "switch", f.branch)
				cliGit(t, f.gateDir, "fetch", f.dir, "+"+unexpected+":refs/heads/"+f.branch)
				return f.liveHead
			},
			wantErr: "at-risk commit",
		},
		{
			name: "conflicting archive",
			mutate: func(t *testing.T, f *returnedCustodyReconcileFixture) string {
				cliGit(t, f.gateDir, "fetch", f.dir, f.liveHead)
				archive := "refs/tags/no-mistakes-abandoned/" + f.branch + "/" + f.returnedHead
				cliGit(t, f.gateDir, "update-ref", archive, f.liveHead)
				return f.liveHead
			},
			wantErr: "archive tag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReturnedCustodyReconcileFixture(t)
			requestedHead := tt.mutate(t, f)
			beforeRefs := cliGit(t, f.gateDir, "for-each-ref", "--format=%(refname) %(objectname)")

			_, err := prepareFreshRunGateBranch(context.Background(), f.env, f.branch, requestedHead)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("unsafe returned-custody evidence: err=%v, want %q", err, tt.wantErr)
			}
			if afterRefs := cliGit(t, f.gateDir, "for-each-ref", "--format=%(refname) %(objectname)"); afterRefs != beforeRefs {
				t.Fatalf("refusal moved gate refs\nbefore:\n%s\nafter:\n%s", beforeRefs, afterRefs)
			}
		})
	}
}

func TestPrepareFreshRunGateBranchRefusesActiveMovedCustody(t *testing.T) {
	f := newReturnedCustodyReconcileFixture(t)
	active, err := f.env.d.InsertRun(f.run.RepoID, f.branch, f.liveHead, f.submitted)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.env.d.UpdateRunHeadSHA(active.ID, f.returnedHead); err != nil {
		t.Fatal(err)
	}
	beforeRefs := cliGit(t, f.gateDir, "for-each-ref", "--format=%(refname) %(objectname)")

	_, err = prepareFreshRunGateBranch(context.Background(), f.env, f.branch, f.liveHead)
	var ownershipErr *branchOwnershipError
	if !errors.As(err, &ownershipErr) || ownershipErr.state.State != "pipeline_owned" {
		t.Fatalf("active moved run was not reported as pipeline-owned: %T %v", err, err)
	}
	if afterRefs := cliGit(t, f.gateDir, "for-each-ref", "--format=%(refname) %(objectname)"); afterRefs != beforeRefs {
		t.Fatalf("active-custody refusal moved gate refs\nbefore:\n%s\nafter:\n%s", beforeRefs, afterRefs)
	}
}

func TestTriggerProofRunRejectedPushRestoresReturnedCustodyGateRef(t *testing.T) {
	f := newReturnedCustodyReconcileFixture(t)
	if err := os.WriteFile(filepath.Join(f.gateDir, "hooks", "pre-receive"), []byte("#!/bin/sh\necho proof-submission-rejected >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if receipt, err := triggerProofRun(ctx, f.env, f.branch, f.liveHead, nil, "preserve monthly coupons", "", "retry-1", "review-retry"); err == nil || receipt != nil || !strings.Contains(err.Error(), "proof-submission-rejected") {
		t.Fatalf("rejected proof submission: receipt=%+v err=%v", receipt, err)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/heads/"+f.branch); got != f.returnedHead {
		t.Fatalf("failed proof submission left gate at %s, want restored %s", got, f.returnedHead)
	}
	archive := "refs/tags/no-mistakes-abandoned/" + f.branch + "/" + f.returnedHead
	if got := cliGit(t, f.gateDir, "rev-parse", archive); got != f.returnedHead {
		t.Fatalf("restoration archive = %s, want %s", got, f.returnedHead)
	}
}

type returnedCustodyReconcileFixture struct {
	dir          string
	gateDir      string
	branch       string
	submitted    string
	returnedHead string
	liveHead     string
	run          *db.Run
	env          *axiEnv
}

func newReturnedCustodyReconcileFixture(t *testing.T) *returnedCustodyReconcileFixture {
	t.Helper()
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
	t.Cleanup(func() { d.Close() })

	branch := "feature/monthly-coupons"
	cliGit(t, dir, "init", "-b", branch)
	cliGit(t, dir, "config", "user.name", "Test")
	cliGit(t, dir, "config", "user.email", "test@example.com")
	write := func(content, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "station.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		cliGit(t, dir, "add", "station.txt")
		cliGit(t, dir, "commit", "-m", message)
		return cliGit(t, dir, "rev-parse", "HEAD")
	}
	base := write("Turbo Station\n", "base")
	submitted := write("Turbo Station\nmonthly coupon\n", "monthly coupon implementation")
	returnedHead := write("Turbo Station\nmonthly coupon\nreviewed behavior\n", "review repair")

	repo, err := d.InsertRepo(dir, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, dir, "clone", "--bare", dir, gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)

	run, err := d.InsertRun(repo.ID, branch, submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, returnedHead); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunCustodyReturned(run.ID); err != nil {
		t.Fatal(err)
	}
	for _, anchorDir := range []string{dir, gateDir} {
		cliGit(t, anchorDir, "update-ref", custody.RecoveryRef(run.ID), returnedHead)
	}

	// Rebase the complete reviewed implementation over a conflicting base
	// advance. Both histories are intentional, but ordinary ancestry and stable
	// patch identity cannot prove that relationship.
	cliGit(t, dir, "reset", "--hard", base)
	write("Turbo Station upstream\n", "advance base")
	liveHead := write("Turbo Station upstream\nmonthly coupon\nreviewed behavior\n", "rebase reviewed monthly coupon implementation")
	if out := cliGit(t, dir, "status", "--porcelain"); out != "" {
		t.Fatalf("fixture worktree is dirty: %s", out)
	}

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodClaimLaunchReceipt, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.ClaimLaunchReceiptResult{Receipt: &ipc.LaunchReceipt{
			RunID: "fresh-run", Disposition: "created", LaunchNonce: "retry-1", ValidationGeneration: "review-retry",
			Branch: branch, HeadSHA: liveHead, SubmittedHeadSHA: liveHead,
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
	chdir(t, dir)

	return &returnedCustodyReconcileFixture{
		dir: dir, gateDir: gateDir, branch: branch, submitted: submitted, returnedHead: returnedHead, liveHead: liveHead, run: run,
		env: &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client},
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
}
