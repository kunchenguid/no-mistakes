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
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

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
