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
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// cliMirrorEnv builds an isolated repo, its bare gate, and an axiEnv bound to
// both, then returns the heads a stranded-mirror scenario needs. The returned
// gate is seeded at mirrorHead, which is the superseded private lineage.
type cliMirrorEnv struct {
	dir        string
	gateDir    string
	repo       *db.Repo
	env        *axiEnv
	base       string
	mirrorHead string
	accepted   string
	localHead  string
	d          *db.DB
}

func newCLIMirrorEnv(t *testing.T, build func(dir string, write func(string, string)) (base string, mirrorHead string, accepted string, localHead string)) *cliMirrorEnv {
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

	cliGit(t, dir, "init", "-b", "main")
	cliGit(t, dir, "config", "user.name", "Test")
	cliGit(t, dir, "config", "user.email", "test@example.com")
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base, mirrorHead, accepted, localHead := build(dir, write)

	repo, err := d.InsertRepo(dir, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, dir, "clone", "--bare", dir, gateDir)
	cliGit(t, gateDir, "update-ref", "refs/heads/main", mirrorHead)

	chdir(t, dir)
	return &cliMirrorEnv{
		dir: dir, gateDir: gateDir, repo: repo, d: d,
		env:  &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig()},
		base: base, mirrorHead: mirrorHead, accepted: accepted, localHead: localHead,
	}
}

// recordTerminalRun records the run that submitted the mirror head, accepted the
// reviewed result, and returned custody.
func (e *cliMirrorEnv) recordTerminalRun(t *testing.T) *db.Run {
	t.Helper()
	run, err := e.d.InsertRun(e.repo.ID, "main", e.mirrorHead, e.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.UpdateRunHeadSHA(run.ID, e.accepted); err != nil {
		t.Fatal(err)
	}
	if err := e.d.UpdateRunReviewApprovedHeadSHA(run.ID, e.accepted); err != nil {
		t.Fatal(err)
	}
	if err := e.d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, e.accepted); err != nil {
		t.Fatal(err)
	}
	if err := e.d.SetRunCustodyReturned(run.ID); err != nil {
		t.Fatal(err)
	}
	return run
}

// TestPreparePrivateMirrorResolvesStrandedMirrorFromRecordedEvidence drives the
// submission path every `axi run` takes. A terminal run that returned custody can
// leave the private mirror on a superseded lineage the containment guard refuses;
// with this repository's own terminal run records the same submission settles it,
// so the operator is not stranded between `axi run`, `rerun`, and `axi sync`.
func TestPreparePrivateMirrorResolvesStrandedMirrorFromRecordedEvidence(t *testing.T) {
	e := newCLIMirrorEnv(t, func(dir string, write func(string, string)) (string, string, string, string) {
		write("base.txt", "base\n")
		cliGit(t, dir, "add", "base.txt")
		cliGit(t, dir, "commit", "-m", "base")
		base := cliGit(t, dir, "rev-parse", "HEAD")

		// The private mirror lineage this run submitted.
		write("feature.txt", "submitted version\n")
		cliGit(t, dir, "add", "feature.txt")
		cliGit(t, dir, "commit", "-m", "submitted work")
		mirrorHead := cliGit(t, dir, "rev-parse", "HEAD")

		// The run's own accepted, reviewed result, then the clean local head.
		cliGit(t, dir, "reset", "--hard", base)
		write("feature.txt", "accepted version\n")
		cliGit(t, dir, "add", "feature.txt")
		cliGit(t, dir, "commit", "-m", "review: accept the fix")
		accepted := cliGit(t, dir, "rev-parse", "HEAD")
		write("followup.txt", "follow-up\n")
		cliGit(t, dir, "add", "followup.txt")
		cliGit(t, dir, "commit", "-m", "follow-up work")
		return base, mirrorHead, accepted, cliGit(t, dir, "rev-parse", "HEAD")
	})
	// The submitted head must be readable in the gate for the guard to compare it.
	cliGit(t, e.dir, "push", e.gateDir, e.mirrorHead+":refs/heads/main")
	run := e.recordTerminalRun(t)

	reconciliation, err := preparePrivateMirror(context.Background(), e.env, "main", e.localHead)
	if err != nil {
		t.Fatalf("submission was refused despite recorded evidence: %v", err)
	}
	if !reconciliation.Reconciled || reconciliation.PreviousHead != e.mirrorHead {
		t.Fatalf("reconciliation = %+v, want the mirror head %s archived", reconciliation, e.mirrorHead)
	}
	if got := cliGit(t, e.gateDir, "rev-parse", reconciliation.ArchivedTag+"^{commit}"); got != e.mirrorHead {
		t.Fatalf("archive = %s, want %s", got, e.mirrorHead)
	}
	// The live head now enters the gate by an ordinary push, which is the point.
	cliGit(t, e.dir, "push", e.gateDir, e.localHead+":refs/heads/main")
	if got := cliGit(t, e.gateDir, "rev-parse", "refs/heads/main"); got != e.localHead {
		t.Fatalf("ordinary push reached %s, want %s", got, e.localHead)
	}
	t.Logf("resolved stranded mirror: run %s submitted %s, accepted %s survived in %s", run.ID, e.mirrorHead, e.accepted, e.localHead)
}

// TestPreparePrivateMirrorRefusalIsActionableAndLeavesTheMirrorUntouched proves
// the preservation guarantee survives at the operator surface: without the
// recorded evidence the same submission refuses, reports the exact condition and
// a concrete action, and moves no ref.
func TestPreparePrivateMirrorRefusalIsActionableAndLeavesTheMirrorUntouched(t *testing.T) {
	e := newCLIMirrorEnv(t, func(dir string, write func(string, string)) (string, string, string, string) {
		write("base.txt", "base\n")
		cliGit(t, dir, "add", "base.txt")
		cliGit(t, dir, "commit", "-m", "base")
		base := cliGit(t, dir, "rev-parse", "HEAD")

		// Private-only work that nothing in the local head contains.
		write("private.txt", "unique private trailer trim\n")
		cliGit(t, dir, "add", "private.txt")
		cliGit(t, dir, "commit", "-m", "private-only change")
		mirrorHead := cliGit(t, dir, "rev-parse", "HEAD")

		cliGit(t, dir, "reset", "--hard", base)
		write("live.txt", "unrelated live work\n")
		cliGit(t, dir, "add", "live.txt")
		cliGit(t, dir, "commit", "-m", "unrelated live work")
		return base, mirrorHead, "", cliGit(t, dir, "rev-parse", "HEAD")
	})

	_, err := preparePrivateMirror(context.Background(), e.env, "main", e.localHead)
	if err == nil {
		t.Fatal("submission reconciled a private head with unproven content")
	}
	mirrorErr, ok := err.(*staleMirrorError)
	if !ok {
		t.Fatalf("refusal was not actionable: %T %v", err, err)
	}
	summary := mirrorErr.Summary()
	for _, want := range []string{"private mirror", e.mirrorHead, e.localHead} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q missing %q", summary, want)
		}
	}
	action := mirrorErr.refusal.Action()
	for _, want := range []string{"no-mistakes axi status", "branch_sync.next_action.command"} {
		if !strings.Contains(action, want) {
			t.Errorf("action %q missing %q", action, want)
		}
	}
	if strings.Contains(action, "no-mistakes rerun") {
		t.Errorf("action %q offers rerun, which this state refuses", action)
	}
	t.Logf("refusal summary: %s", summary)
	t.Logf("refusal action: %s", action)

	if got := cliGit(t, e.gateDir, "rev-parse", "refs/heads/main"); got != e.mirrorHead {
		t.Fatalf("refusal moved the mirror to %s, want %s untouched", got, e.mirrorHead)
	}
	if tags := cliGit(t, e.gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("refusal archived a mirror it did not prove: %q", tags)
	}
	// The refusal must not have created a run or touched the caller's branch.
	if runs, err := e.d.GetRunsByRepo(e.repo.ID); err != nil || len(runs) != 0 {
		t.Fatalf("refusal left run records: %d err=%v", len(runs), err)
	}
	if got := cliGit(t, e.dir, "rev-parse", "HEAD"); got != e.localHead {
		t.Fatalf("refusal moved the caller head to %s, want %s", got, e.localHead)
	}
}

// TestTriggerRunSettlesStrandedMirrorOnRecordedEvidence drives the real
// submission surface end to end: `axi run` on a branch whose private mirror is
// stranded on a superseded lineage. Before this change the submission refused at
// the mirror and the branch could not be revalidated; now the mirror is settled
// from this repository's own records and the caller's head enters the gate by an
// ordinary push. The stub daemon answers no run, so the assertion is the mirror
// state and the push, not a completed run.
func TestTriggerRunSettlesStrandedMirrorOnRecordedEvidence(t *testing.T) {
	e := newCLIMirrorEnv(t, func(dir string, write func(string, string)) (string, string, string, string) {
		write("base.txt", "base\n")
		cliGit(t, dir, "add", "base.txt")
		cliGit(t, dir, "commit", "-m", "base")
		base := cliGit(t, dir, "rev-parse", "HEAD")

		write("feature.txt", "submitted version\n")
		cliGit(t, dir, "add", "feature.txt")
		cliGit(t, dir, "commit", "-m", "submitted work")
		mirrorHead := cliGit(t, dir, "rev-parse", "HEAD")

		cliGit(t, dir, "reset", "--hard", base)
		write("feature.txt", "accepted version\n")
		cliGit(t, dir, "add", "feature.txt")
		cliGit(t, dir, "commit", "-m", "review: accept the fix")
		accepted := cliGit(t, dir, "rev-parse", "HEAD")
		write("followup.txt", "follow-up\n")
		cliGit(t, dir, "add", "followup.txt")
		cliGit(t, dir, "commit", "-m", "follow-up work")
		return base, mirrorHead, accepted, cliGit(t, dir, "rev-parse", "HEAD")
	})
	cliGit(t, e.dir, "push", e.gateDir, e.mirrorHead+":refs/heads/main")
	cliGit(t, e.gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, e.dir, "remote", "add", gate.RemoteName, e.gateDir)
	e.recordTerminalRun(t)

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunsResult{}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{}, nil
	})
	srv.Handle(ipc.MethodRerun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.RerunResult{}, nil
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(e.env.p.Socket()) }()
	t.Cleanup(func() { srv.Close(); <-done })
	var client *ipc.Client
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for {
		client, err = ipc.Dial(e.env.p.Socket())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer client.Close()
	e.env.client = client

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := triggerRun(ctx, e.env, "main", nil, "", ""); err != nil {
		var refusal *staleMirrorError
		if errors.As(err, &refusal) {
			t.Fatalf("submission was refused despite recorded evidence: %v", err)
		}
	}
	// The whole point: the caller's head is now the gate branch, reached by an
	// ordinary push rather than a forced one, and the superseded head is archived.
	if got := cliGit(t, e.gateDir, "rev-parse", "refs/heads/main"); got != e.localHead {
		t.Fatalf("gate branch = %s, want the submitted head %s", got, e.localHead)
	}
	archive := "refs/tags/no-mistakes-abandoned/main/" + e.mirrorHead
	if got := cliGit(t, e.gateDir, "rev-parse", archive); got != e.mirrorHead {
		t.Fatalf("archive = %s, want the superseded head %s", got, e.mirrorHead)
	}
}

// TestStaleMirrorRefusalActionStaysInsideTheGuard pins the action text to the
// guard, so the operator-facing step cannot drift from the condition it answers.
func TestStaleMirrorRefusalActionStaysInsideTheGuard(t *testing.T) {
	refusal := &gate.StaleBranchRefusal{
		BranchRef:    "refs/heads/feature",
		LiveHead:     strings.Repeat("a", 40),
		PreviousHead: strings.Repeat("b", 40),
		AtRisk:       []string{strings.Repeat("b", 40) + " private work"},
	}
	if got := refusal.Error(); !strings.Contains(got, "at-risk commit") || !strings.Contains(got, strings.Repeat("b", 40)) {
		t.Fatalf("Error() = %q, want the at-risk commit named", got)
	}
	if action := refusal.Action(); action == "" {
		t.Fatal("Action() is empty, so the refusal is not actionable")
	}
}
