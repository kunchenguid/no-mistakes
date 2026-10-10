package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// releasedRewriteGate is the shape behind issues #1066 and #1063: a run
// validated the submitted head, went terminal without publishing (the push
// step was skipped, as a Gerrit-reviewed workflow does), and the operator then
// rewrote the branch for its next revision. The gate's branch ref still names
// the submitted head, which is not an ancestor of the rewritten head.
type releasedRewriteGate struct {
	dir       string
	gateDir   string
	base      string
	submitted string
	rewritten string
	runID     string
	env       *axiEnv
	done      chan error
}

func newReleasedRewriteGate(t *testing.T, status types.RunStatus, handlers map[string]func(context.Context, json.RawMessage) (interface{}, error)) *releasedRewriteGate {
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
	cliGit(t, dir, "commit", "--allow-empty", "-m", "base")
	base := cliGit(t, dir, "rev-parse", "HEAD")
	write := func(name, content, message string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		cliGit(t, dir, "add", name)
		cliGit(t, dir, "commit", "-m", message)
	}
	write("feature.txt", "feature\n", "add feature")
	submitted := cliGit(t, dir, "rev-parse", "HEAD")
	repo, err := d.InsertRepo(dir, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, dir, "clone", "--bare", dir, gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)

	// The run that released the branch: terminal, never published, and its
	// verified head never moved off the submitted head.
	run, err := d.InsertRun(repo.ID, "main", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(run.ID, status, submitted); err != nil {
		t.Fatal(err)
	}

	// The next revision: the same file rewritten with different content on
	// the same base, so the new head is neither an ancestor nor a descendant
	// of the submitted head and its patch no longer matches.
	cliGit(t, dir, "reset", "--hard", base)
	write("feature.txt", "feature, revised\n", "add feature (revised)")
	rewritten := cliGit(t, dir, "rev-parse", "HEAD")

	srv := ipc.NewServer()
	for method, handler := range handlers {
		srv.Handle(method, handler)
	}
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
	return &releasedRewriteGate{
		dir: dir, gateDir: gateDir, base: base, submitted: submitted, rewritten: rewritten, runID: run.ID,
		env:  &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client},
		done: done,
	}
}

// recordPushOptions installs a post-receive hook that writes every push option
// the gate received, so a test can prove the archived previous head rode along
// with the submission.
func (g *releasedRewriteGate) recordPushOptions(t *testing.T) string {
	t.Helper()
	log := filepath.Join(g.gateDir, "push-options.log")
	hook := "#!/bin/sh\ni=0\nwhile [ \"$i\" -lt \"${GIT_PUSH_OPTION_COUNT:-0}\" ]; do\n  printenv \"GIT_PUSH_OPTION_$i\" >> '" + log + "'\n  i=$((i + 1))\ndone\nexit 0\n"
	if err := os.WriteFile(filepath.Join(g.gateDir, "hooks", "post-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

// gateBranchIs reports whether the gate's main branch currently names head,
// without failing the test from a non-test goroutine.
func (g *releasedRewriteGate) gateBranchIs(head string) bool {
	got, err := git.Run(context.Background(), g.gateDir, "rev-parse", "--verify", "refs/heads/main")
	return err == nil && got == head
}

func noActiveRun(context.Context, json.RawMessage) (interface{}, error) {
	return &ipc.GetActiveRunResult{}, nil
}

func noRunsForHead(context.Context, json.RawMessage) (interface{}, error) {
	return &ipc.GetRunsResult{}, nil
}

func TestTriggerRunArchivesReleasedRunSubmittedMirrorForRewrittenBranch(t *testing.T) {
	var g *releasedRewriteGate
	g = newReleasedRewriteGate(t, types.RunCompleted, map[string]func(context.Context, json.RawMessage) (interface{}, error){
		ipc.MethodGetRunsForHead: noRunsForHead,
		// The daemon registers the fresh run once the push lands on the
		// reconciled lane; before that the gate's branch is still the stale
		// submitted head and there is nothing to report.
		ipc.MethodGetActiveRun: func(context.Context, json.RawMessage) (interface{}, error) {
			if g != nil && g.gateBranchIs(g.rewritten) {
				return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{ID: "run-rewritten", Branch: "main", HeadSHA: g.rewritten, Status: types.RunRunning}}, nil
			}
			return &ipc.GetActiveRunResult{}, nil
		},
	})
	optionsLog := g.recordPushOptions(t)

	// The submission would be rejected as non-fast-forward by a plain push, and
	// refused as at-risk content by the containment proof: the rewritten patch
	// is different content on the same base.
	if _, err := git.Run(context.Background(), g.gateDir, "merge-base", "--is-ancestor", g.submitted, g.rewritten); err == nil {
		t.Fatalf("fixture did not rewrite history: %s descends from %s", g.rewritten, g.submitted)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runID, err := triggerRun(ctx, g.env, "main", nil, "", "", false, "", nil)
	if err != nil || runID != "run-rewritten" {
		t.Fatalf("fresh submission of the rewritten released branch: run=%q err=%v", runID, err)
	}
	if got := cliGit(t, g.gateDir, "rev-parse", "refs/heads/main"); got != g.rewritten {
		t.Fatalf("gate branch = %s, want rewritten head %s", got, g.rewritten)
	}
	if got := cliGit(t, g.gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/main/"+g.submitted); got != g.submitted {
		t.Fatalf("archive tag = %s, want the submitted head %s archived before replacement", got, g.submitted)
	}
	options, readErr := os.ReadFile(optionsLog)
	if readErr != nil {
		t.Fatalf("read recorded push options: %v", readErr)
	}
	if !strings.Contains(string(options), "no-mistakes.reconciled-previous-head="+g.submitted) {
		t.Fatalf("submission did not carry the archived head as the run's previous head:\n%s", options)
	}
	if got := cliGit(t, g.dir, "rev-parse", "HEAD"); got != g.rewritten {
		t.Fatalf("submission moved the caller's head to %s, want %s", got, g.rewritten)
	}
}

func TestTriggerRunRefusesReleasedRunMirrorThatMovedPastTheSubmittedHead(t *testing.T) {
	g := newReleasedRewriteGate(t, types.RunCompleted, map[string]func(context.Context, json.RawMessage) (interface{}, error){
		ipc.MethodGetRunsForHead: noRunsForHead,
		ipc.MethodGetActiveRun:   noActiveRun,
	})
	// The gate lane advanced past the released run's submitted head without a
	// run of its own, so the lane head is no longer the operator's exact
	// submission and the ownership allowance must not apply.
	cliGit(t, g.dir, "checkout", "-q", "--detach", g.submitted)
	if err := os.WriteFile(filepath.Join(g.dir, "extra.txt"), []byte("extra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, g.dir, "add", "extra.txt")
	cliGit(t, g.dir, "commit", "-m", "extra commit on the lane")
	laneHead := cliGit(t, g.dir, "rev-parse", "HEAD")
	cliGit(t, g.dir, "push", gate.RemoteName, "HEAD:refs/heads/main")
	cliGit(t, g.dir, "checkout", "-q", "main")
	if got := cliGit(t, g.dir, "rev-parse", "HEAD"); got != g.rewritten {
		t.Fatalf("caller head = %s, want rewritten %s", got, g.rewritten)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runID, err := triggerRun(ctx, g.env, "main", nil, "", "", false, "", nil)
	if err == nil || runID != "" || !strings.Contains(err.Error(), "at-risk commit") {
		t.Fatalf("lane that moved past the submitted head must keep the at-risk refusal: run=%q err=%v", runID, err)
	}
	if got := cliGit(t, g.gateDir, "rev-parse", "refs/heads/main"); got != laneHead {
		t.Fatalf("refusal moved the gate branch to %s, want untouched %s", got, laneHead)
	}
	if _, tagErr := git.Run(ctx, g.gateDir, "rev-parse", "--verify", "refs/tags/no-mistakes-abandoned/main/"+laneHead); tagErr == nil {
		t.Fatalf("refusal archived the lane head %s", laneHead)
	}
	if _, tagErr := git.Run(ctx, g.gateDir, "rev-parse", "--verify", "refs/tags/no-mistakes-abandoned/main/"+g.submitted); tagErr == nil {
		t.Fatalf("refusal archived the submitted head %s", g.submitted)
	}
}

func TestTriggerRunRefusesCustodyReturnedRunMirrorForRewrittenBranch(t *testing.T) {
	g := newReleasedRewriteGate(t, types.RunCompleted, map[string]func(context.Context, json.RawMessage) (interface{}, error){
		ipc.MethodGetRunsForHead: noRunsForHead,
		ipc.MethodGetActiveRun:   noActiveRun,
	})
	// Custody was returned rather than the branch released, so the exact-head
	// allowance does not apply even though the lane still names the submitted
	// head and the local branch was rewritten.
	if err := g.env.d.SetRunCustodyReturned(g.runID); err != nil {
		t.Fatal(err)
	}
	if got := cliGit(t, g.gateDir, "rev-parse", "refs/heads/main"); got != g.submitted {
		t.Fatalf("gate branch = %s, want the submitted head %s", got, g.submitted)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runID, err := triggerRun(ctx, g.env, "main", nil, "", "", false, "", nil)
	if err == nil || runID != "" || !strings.Contains(err.Error(), "at-risk commit") {
		t.Fatalf("custody-returned rewrite must keep the at-risk refusal: run=%q err=%v", runID, err)
	}
	if got := cliGit(t, g.gateDir, "rev-parse", "refs/heads/main"); got != g.submitted {
		t.Fatalf("refusal moved the gate branch to %s, want untouched %s", got, g.submitted)
	}
	if tags := cliGit(t, g.gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("refusal created archive tags:\n%s", tags)
	}
}

func TestTriggerProofRunArchivesReleasedRunSubmittedMirrorForRewrittenBranch(t *testing.T) {
	const nonce, generation = "nonce-released-rewrite", "generation-1"
	var g *releasedRewriteGate
	g = newReleasedRewriteGate(t, types.RunCancelled, map[string]func(context.Context, json.RawMessage) (interface{}, error){
		ipc.MethodGetRunsForHead: noRunsForHead,
		ipc.MethodGetActiveRun:   noActiveRun,
		ipc.MethodClaimLaunchReceipt: func(context.Context, json.RawMessage) (interface{}, error) {
			if g != nil && g.gateBranchIs(g.rewritten) {
				return &ipc.ClaimLaunchReceiptResult{Receipt: &ipc.LaunchReceipt{
					RunID: "run-proof", Disposition: "created", LaunchNonce: nonce, ValidationGeneration: generation,
					Branch: "main", HeadSHA: g.rewritten, SubmittedHeadSHA: g.rewritten,
				}}, nil
			}
			return &ipc.ClaimLaunchReceiptResult{}, nil
		},
	})
	optionsLog := g.recordPushOptions(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	receipt, err := triggerProofRun(ctx, g.env, "main", g.rewritten, nil, "", "", false, nonce, generation, "", nil)
	if err != nil || receipt == nil || receipt.RunID != "run-proof" {
		t.Fatalf("proof submission of the rewritten released branch: receipt=%+v err=%v", receipt, err)
	}
	if got := cliGit(t, g.gateDir, "rev-parse", "refs/heads/main"); got != g.rewritten {
		t.Fatalf("gate branch = %s, want rewritten head %s", got, g.rewritten)
	}
	if got := cliGit(t, g.gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/main/"+g.submitted); got != g.submitted {
		t.Fatalf("archive tag = %s, want the submitted head %s archived before replacement", got, g.submitted)
	}
	options, readErr := os.ReadFile(optionsLog)
	if readErr != nil {
		t.Fatalf("read recorded push options: %v", readErr)
	}
	if !strings.Contains(string(options), "no-mistakes.reconciled-previous-head="+g.submitted) {
		t.Fatalf("proof submission did not carry the archived head as the run's previous head:\n%s", options)
	}
}

func TestTriggerProofRunRejectedPushRestoresReleasedRunMirror(t *testing.T) {
	g := newReleasedRewriteGate(t, types.RunCompleted, map[string]func(context.Context, json.RawMessage) (interface{}, error){
		ipc.MethodGetRunsForHead: noRunsForHead,
		ipc.MethodGetActiveRun:   noActiveRun,
	})
	if err := os.WriteFile(filepath.Join(g.gateDir, "hooks", "pre-receive"), []byte("#!/bin/sh\necho submission-rejected >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	receipt, err := triggerProofRun(ctx, g.env, "main", g.rewritten, nil, "", "", false, "nonce-rejected", "generation-1", "", nil)
	if err == nil || receipt != nil || !strings.Contains(err.Error(), "submission-rejected") {
		t.Fatalf("rejected proof submission: receipt=%+v err=%v", receipt, err)
	}
	if got := cliGit(t, g.gateDir, "rev-parse", "refs/heads/main"); got != g.submitted {
		t.Fatalf("failed submission left the gate branch at %s, want the restored submitted head %s", got, g.submitted)
	}
	if got := cliGit(t, g.gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/main/"+g.submitted); got != g.submitted {
		t.Fatalf("archive tag = %s, want %s", got, g.submitted)
	}
	if got := cliGit(t, g.dir, "rev-parse", "HEAD"); got != g.rewritten {
		t.Fatalf("failed submission moved the caller's head to %s, want %s", got, g.rewritten)
	}
}

func TestReleasedRunSubmittedHeadForFreshRun(t *testing.T) {
	const submitted, rewritten = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	released := func() branchsync.State {
		return branchsync.State{
			State:    branchsync.StateUserOwned,
			Relation: branchsync.RelationDiverged,
			Local:    branchsync.LocalState{Branch: "main", Head: rewritten, Clean: true},
			Pipeline: branchsync.PipelineState{Status: string(types.RunCompleted), SubmittedHead: submitted, CurrentHead: submitted},
		}
	}
	for _, tt := range []struct {
		name   string
		mutate func(*branchsync.State)
		head   string
		want   string
	}{
		{name: "released and rewritten", mutate: func(*branchsync.State) {}, head: rewritten, want: submitted},
		{name: "custody returned with the same evidence", mutate: func(s *branchsync.State) { s.State = branchsync.StateCustodyReturned }, head: rewritten},
		{name: "cancelled run releases too", mutate: func(s *branchsync.State) { s.Pipeline.Status = string(types.RunCancelled) }, head: rewritten, want: submitted},
		{name: "head moved since inspection", mutate: func(*branchsync.State) {}, head: "3333333333333333333333333333333333333333", want: ""},
		{name: "other branch", mutate: func(s *branchsync.State) { s.Local.Branch = "other" }, head: rewritten, want: ""},
		{name: "dirty worktree", mutate: func(s *branchsync.State) { s.Local.Clean = false }, head: rewritten, want: ""},
		{name: "behind is not a rewrite", mutate: func(s *branchsync.State) { s.Relation = branchsync.RelationBehind }, head: rewritten, want: ""},
		{name: "ahead needs no allowance", mutate: func(s *branchsync.State) { s.Relation = branchsync.RelationAhead }, head: rewritten, want: ""},
		{name: "pipeline owned", mutate: func(s *branchsync.State) { s.State = branchsync.StatePipelineOwned }, head: rewritten, want: ""},
		{name: "active run", mutate: func(s *branchsync.State) { s.Pipeline.Status = string(types.RunRunning) }, head: rewritten, want: ""},
		{name: "run moved its head", mutate: func(s *branchsync.State) { s.Pipeline.CurrentHead = "4444444444444444444444444444444444444444" }, head: rewritten, want: ""},
		{name: "run published", mutate: func(s *branchsync.State) { s.Pipeline.PushedHead = submitted }, head: rewritten, want: ""},
		{name: "no submitted head recorded", mutate: func(s *branchsync.State) { s.Pipeline.SubmittedHead = ""; s.Pipeline.CurrentHead = "" }, head: rewritten, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := released()
			tt.mutate(&state)
			if got := releasedRunSubmittedHeadForFreshRun(state, "main", tt.head); got != tt.want {
				t.Fatalf("releasedRunSubmittedHeadForFreshRun = %q, want %q", got, tt.want)
			}
		})
	}
}
