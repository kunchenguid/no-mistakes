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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTriggerRunWithoutMirrorDoesNotQueryUpstream(t *testing.T) {
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
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	cliGit(t, dir, "clone", "--bare", dir, upstream)
	cliGit(t, dir, "remote", "add", "origin", upstream)
	cliGit(t, dir, "checkout", "-b", "feature")
	cliGit(t, dir, "commit", "--allow-empty", "-m", "feature")
	head := cliGit(t, dir, "rev-parse", "HEAD")
	repo, err := d.InsertRepo(dir, upstream, "main")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, dir, "init", "--bare", gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)
	if refs := cliGit(t, gateDir, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatalf("fresh mirror already has refs: %s", refs)
	}

	srv := ipc.NewServer()
	serveTranscriptHomesProbe(srv)
	srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunsResult{}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{
			ID:      "01M3TRIGGEREDRUN00000000000",
			RepoID:  repo.ID,
			Branch:  "feature",
			HeadSHA: head,
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
	trace := filepath.Join(t.TempDir(), "git.trace")
	t.Setenv("GIT_TRACE", trace)
	env := &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runID, err := triggerRun(ctx, env, "feature", nil, "submit local work", "", false, "", nil)
	if err != nil || runID != "01M3TRIGGEREDRUN00000000000" {
		t.Fatalf("fresh submission: run=%q err=%v", runID, err)
	}
	if got := cliGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatalf("submitted mirror = %s, want %s", got, head)
	}
	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		// Submission still checks its private gate; only upstream discovery
		// and fetching must remain lazy when that gate has no branch mirror.
		upstreamQuery := strings.Contains(line, " ls-remote ") && !strings.Contains(line, " ls-remote "+gate.RemoteName+" ")
		if upstreamQuery || strings.Contains(line, " fetch ") || strings.Contains(line, " remote get-url origin") {
			t.Fatalf("fresh submission queried upstream: %s", line)
		}
	}
}
