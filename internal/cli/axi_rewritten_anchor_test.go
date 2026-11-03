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

// Recovery writes the superseded push anchor in the invoking worktree when
// that object is available there. A subsequent fresh submission must consult
// that evidence, not require a hand-made recovery ref in the gate.
func TestTriggerRunAfterRemoteRewrittenRecovery(t *testing.T) {
	f := newCLISyncFixture(t)
	if out, err := executeCmd("axi", "sync"); err != nil {
		t.Fatalf("initial sync: %v\n%s", err, out)
	}
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	// Keep the IPC endpoint short enough for macOS's Unix socket limit.
	socketPaths := paths.WithRoot(makeSocketSafeTempDir(t))
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.GetRepoByPath(f.local)
	if err != nil || repo == nil {
		t.Fatalf("get fixture repo: %v", err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, f.local, "clone", "--bare", f.local, gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, f.local, "remote", "add", gate.RemoteName, gateDir)

	cliGit(t, f.local, "reset", "--hard", f.base)
	if err := os.WriteFile(filepath.Join(f.local, "live.txt"), []byte("rewritten work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, f.local, "add", "live.txt")
	cliGit(t, f.local, "commit", "-m", "rewrite published branch")
	liveHead := cliGit(t, f.local, "rev-parse", "HEAD")
	cliGit(t, f.local, "push", "--force-with-lease=refs/heads/feature/sync:"+f.pushed, f.remote, "HEAD:refs/heads/feature/sync")
	out, err := executeCmd("axi", "sync", "--recover")
	if err != nil || !strings.Contains(out, "recovered: true") || !strings.Contains(out, "proof: worktree") {
		t.Fatalf("recover rewritten remote: %v\n%s", err, out)
	}
	anchor := "refs/no-mistakes/recover-rewritten/" + f.runID + "/1"
	if got := cliGit(t, f.local, "rev-parse", anchor); got != f.pushed {
		t.Fatalf("worktree anchor = %s, want %s", got, f.pushed)
	}
	if got := cliGit(t, gateDir, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/"); got != "" {
		t.Fatalf("fixture must have only a worktree anchor, found gate refs: %s", got)
	}

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunsResult{}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{
			ID: "01MREWRITTENTRIGGER000000000", RepoID: repo.ID,
			Branch: "feature/sync", HeadSHA: liveHead, Status: types.RunRunning,
		}}, nil
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(socketPaths.Socket()) }()
	t.Cleanup(func() { srv.Close(); <-done })
	var client *ipc.Client
	deadline := time.Now().Add(3 * time.Second)
	for {
		client, err = ipc.Dial(socketPaths.Socket())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer client.Close()
	env := &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runID, err := triggerRun(ctx, env, "feature/sync", nil, "Validate rewritten branch", "", false, "", nil)
	if err != nil || runID == "" {
		t.Fatalf("submission after successful recovery refused: run=%q err=%v\n%s", runID, err, out)
	}
	for dir, ref := range map[string]string{
		f.local: anchor,
		gateDir: "refs/tags/no-mistakes-abandoned/feature/sync/" + f.pushed,
	} {
		if got := cliGit(t, dir, "rev-parse", ref); got != f.pushed {
			t.Fatalf("preserved ref %s in %s = %s, want %s", ref, dir, got, f.pushed)
		}
	}
	if got := cliGit(t, gateDir, "rev-parse", "refs/heads/feature/sync"); got != liveHead {
		t.Fatalf("submitted mirror = %s, want %s", got, liveHead)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != liveHead {
		t.Fatalf("submission moved the worktree to %s", got)
	}
	t.Logf("Recovery -> fresh run %s: submitted %s; preserved %s at %s and exact mirror archive", runID, liveHead, f.pushed, anchor)
}
