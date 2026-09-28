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
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type returnedCustodyStaleSubmissionFixture struct {
	p         *paths.Paths
	d         *db.DB
	repo      *db.Repo
	local     string
	gateDir   string
	branch    string
	runID     string
	submitted string
	preserved string
	localHead string
}

func newReturnedCustodyStaleSubmissionFixture(t *testing.T, dropSubmittedLine bool) returnedCustodyStaleSubmissionFixture {
	t.Helper()
	root := t.TempDir()
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

	remote := filepath.Join(root, "remote.git")
	cliGit(t, root, "init", "--bare", remote)
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(local, "report.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "report.txt")
	cliGit(t, local, "commit", "-m", "base")
	base := cliGit(t, local, "rev-parse", "HEAD")

	branch := "feature/recover"
	cliGit(t, local, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(local, "report.txt"), []byte("base\napproved one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "report.txt")
	cliGit(t, local, "commit", "-m", "submitted report part one")
	if err := os.WriteFile(filepath.Join(local, "report.txt"), []byte("base\napproved one\napproved two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "report.txt")
	cliGit(t, local, "commit", "-m", "submitted report part two")
	submitted := cliGit(t, local, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(local, "pipeline.txt"), []byte("pipeline-only cancelled work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "pipeline.txt")
	cliGit(t, local, "commit", "-m", "no-mistakes(review): cancelled fix")
	preserved := cliGit(t, local, "rev-parse", "HEAD")

	cliGit(t, local, "reset", "--hard", base)
	replacement := "base\napproved one\napproved two\n"
	if dropSubmittedLine {
		replacement = "base\napproved one\n"
	}
	if err := os.WriteFile(filepath.Join(local, "report.txt"), []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "report.txt")
	cliGit(t, local, "commit", "-m", "replacement history")
	localHead := cliGit(t, local, "rev-parse", "HEAD")

	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo(registeredRoot, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, branch, submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, preserved); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunCustodyReturned(run.ID); err != nil {
		t.Fatal(err)
	}

	gateDir := p.RepoDir(repo.ID)
	cliGit(t, filepath.Dir(gateDir), "init", "--bare", gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, local, "remote", "add", gate.RemoteName, gateDir)
	cliGit(t, local, "push", gateDir, submitted+":refs/heads/"+branch, preserved+":refs/no-mistakes/recover/"+run.ID)
	chdir(t, local)
	return returnedCustodyStaleSubmissionFixture{p: p, d: d, repo: repo, local: local, gateDir: gateDir, branch: branch, runID: run.ID, submitted: submitted, preserved: preserved, localHead: localHead}
}

func startFreshRunTriggerServer(t *testing.T, p *paths.Paths, f returnedCustodyStaleSubmissionFixture) *ipc.Client {
	t.Helper()
	srv := ipc.NewServer()
	fresh := &ipc.RunInfo{ID: "fresh-run", RepoID: f.repo.ID, Branch: f.branch, HeadSHA: f.localHead, Status: types.RunRunning}
	currentFreshRun := func() *ipc.RunInfo {
		if head, err := gitHead(context.Background(), f.gateDir, "refs/heads/"+f.branch); err == nil && head == f.localHead {
			return fresh
		}
		return nil
	}
	srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
		if run := currentFreshRun(); run != nil {
			return &ipc.GetRunsResult{Runs: []ipc.RunInfo{*run}}, nil
		}
		return &ipc.GetRunsResult{}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{Run: currentFreshRun()}, nil
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() { srv.Close(); <-done })
	var client *ipc.Client
	var err error
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
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func gitHead(ctx context.Context, dir, ref string) (string, error) {
	return git.Run(ctx, dir, "rev-parse", ref+"^{commit}")
}

func startProofRunTriggerServer(t *testing.T, p *paths.Paths, f returnedCustodyStaleSubmissionFixture, launchNonce, generation, intent string) *ipc.Client {
	t.Helper()
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodClaimLaunchReceipt, func(context.Context, json.RawMessage) (interface{}, error) {
		if head, err := gitHead(context.Background(), f.gateDir, "refs/heads/"+f.branch); err == nil && head == f.localHead {
			return &ipc.ClaimLaunchReceiptResult{Receipt: &ipc.LaunchReceipt{
				RunID: "proof-run", Disposition: "started", LaunchNonce: launchNonce,
				ValidationGeneration: generation, Branch: f.branch, HeadSHA: f.localHead,
				SubmittedHeadSHA: f.localHead, IntentDigest: digestLaunchIntent(intent),
			}}, nil
		}
		return &ipc.ClaimLaunchReceiptResult{}, nil
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() { srv.Close(); <-done })
	var client *ipc.Client
	var err error
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
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestTriggerRunReconcilesReturnedCustodySubmittedMirrorForContentPreservingReplacement(t *testing.T) {
	f := newReturnedCustodyStaleSubmissionFixture(t, false)
	status, err := executeCmd("axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	for _, want := range []string{"state: custody_returned", "relation: diverged", "safety: stale_mirror_reconcilable", "code: run_pipeline"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status missing %q:\n%s", want, status)
		}
	}
	if strings.Contains(status, "adopt_published") {
		t.Fatalf("status offered the impossible adoption path:\n%s", status)
	}

	client := startFreshRunTriggerServer(t, f.p, f)
	env := &axiEnv{p: f.p, d: f.d, repo: f.repo, cfg: config.DefaultGlobalConfig(), client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runID, err := triggerRun(ctx, env, f.branch, nil, "intent", "", false, "")
	if err != nil || runID != "fresh-run" {
		t.Fatalf("trigger fresh run: run=%q err=%v", runID, err)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/heads/"+f.branch); got != f.localHead {
		t.Fatalf("gate branch = %s, want replacement head %s", got, f.localHead)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/"+f.branch+"/"+f.submitted); got != f.submitted {
		t.Fatalf("submitted head archive = %s, want %s", got, f.submitted)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/no-mistakes/recover/"+f.runID+"^{commit}"); got != f.preserved {
		t.Fatalf("pipeline recovery ref = %s, want %s", got, f.preserved)
	}
}

func TestTriggerProofRunReconcilesReturnedCustodySubmittedMirrorForContentPreservingReplacement(t *testing.T) {
	f := newReturnedCustodyStaleSubmissionFixture(t, false)
	const (
		launchNonce = "proof-nonce"
		generation  = "proof-generation"
		intent      = "intent"
	)
	client := startProofRunTriggerServer(t, f.p, f, launchNonce, generation, intent)
	env := &axiEnv{p: f.p, d: f.d, repo: f.repo, cfg: config.DefaultGlobalConfig(), client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, err := triggerProofRun(ctx, env, f.branch, f.localHead, nil, intent, "", false, launchNonce, generation, "")
	if err != nil || receipt == nil || receipt.RunID != "proof-run" {
		t.Fatalf("trigger strict fresh run: receipt=%+v err=%v", receipt, err)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/heads/"+f.branch); got != f.localHead {
		t.Fatalf("gate branch = %s, want replacement head %s", got, f.localHead)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/"+f.branch+"/"+f.submitted); got != f.submitted {
		t.Fatalf("submitted head archive = %s, want %s", got, f.submitted)
	}
}

func TestTriggerProofRunRejectedPushRestoresReturnedCustodyMirror(t *testing.T) {
	f := newReturnedCustodyStaleSubmissionFixture(t, false)
	if err := os.WriteFile(filepath.Join(f.gateDir, "hooks", "pre-receive"), []byte("#!/bin/sh\necho submission-rejected >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const (
		launchNonce = "proof-nonce"
		generation  = "proof-generation"
		intent      = "intent"
	)
	client := startProofRunTriggerServer(t, f.p, f, launchNonce, generation, intent)
	env := &axiEnv{p: f.p, d: f.d, repo: f.repo, cfg: config.DefaultGlobalConfig(), client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, err := triggerProofRun(ctx, env, f.branch, f.localHead, nil, intent, "", false, launchNonce, generation, "")
	if err == nil || receipt != nil || !strings.Contains(err.Error(), "submission-rejected") {
		t.Fatalf("rejected strict submission: receipt=%+v err=%v", receipt, err)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/heads/"+f.branch); got != f.submitted {
		t.Fatalf("failed strict submission left mirror at %s, want restored %s", got, f.submitted)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/"+f.branch+"/"+f.submitted); got != f.submitted {
		t.Fatalf("archive = %s, want %s", got, f.submitted)
	}
}

func TestCustodyReturnedStatusRejectsUnplannableGateRefs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, f returnedCustodyStaleSubmissionFixture)
	}{
		{
			name: "symbolic branch",
			mutate: func(t *testing.T, f returnedCustodyStaleSubmissionFixture) {
				cliGit(t, f.gateDir, "update-ref", "refs/heads/symbolic-target", f.submitted)
				cliGit(t, f.gateDir, "symbolic-ref", "refs/heads/"+f.branch, "refs/heads/symbolic-target")
			},
		},
		{
			name: "symbolic archive",
			mutate: func(t *testing.T, f returnedCustodyStaleSubmissionFixture) {
				cliGit(t, f.gateDir, "update-ref", "refs/heads/archive-target", f.submitted)
				archiveRef := "refs/tags/no-mistakes-abandoned/" + f.branch + "/" + f.submitted
				cliGit(t, f.gateDir, "symbolic-ref", archiveRef, "refs/heads/archive-target")
			},
		},
		{
			name: "conflicting archive",
			mutate: func(t *testing.T, f returnedCustodyStaleSubmissionFixture) {
				// The replacement commit is intentionally not reachable from the stale
				// gate lane, so import its object before installing the conflicting ref.
				cliGit(t, f.gateDir, "fetch", f.local, f.localHead)
				archiveRef := "refs/tags/no-mistakes-abandoned/" + f.branch + "/" + f.submitted
				cliGit(t, f.gateDir, "update-ref", archiveRef, f.localHead)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReturnedCustodyStaleSubmissionFixture(t, false)
			tt.mutate(t, f)
			status, err := executeCmd("axi", "status")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, status)
			}
			if strings.Contains(status, "safety: stale_mirror_reconcilable") || strings.Contains(status, "code: run_pipeline") {
				t.Fatalf("status offered reconciliation for an unplannable gate ref:\n%s", status)
			}
			if !strings.Contains(status, "code: adopt_published") {
				t.Fatalf("status did not retain the safe fallback:\n%s", status)
			}
		})
	}
}

func TestTriggerRunRefusesReturnedCustodySubmittedMirrorWithUniqueAtRiskContent(t *testing.T) {
	f := newReturnedCustodyStaleSubmissionFixture(t, true)
	status, err := executeCmd("axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	if strings.Contains(status, "safety: stale_mirror_reconcilable") || strings.Contains(status, "code: run_pipeline") {
		t.Fatalf("status treated missing submitted content as runnable:\n%s", status)
	}

	client := startFreshRunTriggerServer(t, f.p, f)
	env := &axiEnv{p: f.p, d: f.d, repo: f.repo, cfg: config.DefaultGlobalConfig(), client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runID, err := triggerRun(ctx, env, f.branch, nil, "intent", "", false, "")
	if err == nil || runID != "" || !strings.Contains(err.Error(), "refusing to reconcile private mirror ref") || !strings.Contains(err.Error(), "at-risk") {
		t.Fatalf("unique submitted content was not refused: run=%q err=%v", runID, err)
	}
	if got := cliGit(t, f.gateDir, "rev-parse", "refs/heads/"+f.branch); got != f.submitted {
		t.Fatalf("refusal moved gate branch to %s, want submitted head %s", got, f.submitted)
	}
	if tags := cliGit(t, f.gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("refusal archived at-risk content for deletion: %s", tags)
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
	runID, err := triggerRun(ctx, env, "main", nil, "", "", false, "")
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
