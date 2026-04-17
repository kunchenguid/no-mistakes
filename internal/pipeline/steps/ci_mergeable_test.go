package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestCIStep_TimeoutDoesNotSleepPastDeadline(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 2 * time.Second

	started := time.Unix(1700000000, 0)
	now := started
	var intervals []time.Duration

	step := &CIStep{
		now: func() time.Time {
			return now
		},
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			intervals = append(intervals, interval)
			now = now.Add(interval)
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when timeout expires")
	}
	if len(intervals) != 1 {
		t.Fatalf("expected exactly one poll wait before timeout, got %d", len(intervals))
	}
	if intervals[0] != 2*time.Second {
		t.Fatalf("wait interval = %v, want clipped timeout %v", intervals[0], 2*time.Second)
	}
}

func TestCIStep_UnknownMergeableStateDoesNotExitCleanly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "UNKNOWN")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}

	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected polling to continue until canceled, got %v", err)
	}

	for _, l := range logs {
		if strings.Contains(l, "all CI checks passed") {
			t.Fatalf("expected UNKNOWN mergeability to block clean exit, got logs: %v", logs)
		}
	}
}

func TestCIStep_MergeableLookupErrorStillExitsCleanlyWhenChecksPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGHMergeableError(t, "OPEN", checksJSON, "gh mergeable failed")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 10 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected clean exit, got %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected clean outcome, got approval request: %+v", outcome)
	}

	foundWarning := false
	foundPassed := false
	for _, l := range logs {
		if strings.Contains(l, "could not check mergeable state") {
			foundWarning = true
		}
		if strings.Contains(l, "all CI checks passed") {
			foundPassed = true
		}
	}
	if !foundWarning {
		t.Fatalf("expected mergeable lookup warning, got logs: %v", logs)
	}
	if !foundPassed {
		t.Fatalf("expected clean-pass log after mergeable lookup warning, got logs: %v", logs)
	}
}

func TestCIStep_TimeoutWithUnknownMergeableState_NeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "UNKNOWN")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 2 * time.Second

	started := time.Unix(1700000000, 0)
	now := started
	step := &CIStep{
		now: func() time.Time {
			return now
		},
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			now = now.Add(interval)
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval when mergeability stays unknown until timeout")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}

	found := false
	for _, item := range findings.Items {
		if strings.Contains(strings.ToLower(item.Description), "mergeable") || strings.Contains(strings.ToLower(item.Description), "mergeability") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected unresolved mergeability finding, got %+v", findings.Items)
	}
}

func TestCIStep_TimeoutWithKnownFailureAndPendingCheck_NeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[{"name":"build","state":"FAILURE","bucket":"fail"},{"name":"test","state":"PENDING","bucket":"pending"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 2 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	started := time.Unix(1700000000, 0)
	now := started
	step := &CIStep{
		now: func() time.Time {
			return now
		},
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			now = now.Add(interval)
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval when known CI issues remain at timeout")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}

	foundFailure := false
	foundConflict := false
	for _, item := range findings.Items {
		desc := strings.ToLower(item.Description)
		if strings.Contains(desc, "build") {
			foundFailure = true
		}
		if strings.Contains(desc, "merge conflict") {
			foundConflict = true
		}
	}
	if !foundFailure || !foundConflict {
		t.Fatalf("expected failing check and merge conflict findings, got %+v", findings.Items)
	}
}

func TestCIStep_TimeoutWithMergeConflictAndCheckLookupError_NeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGHChecksError(t, "OPEN", "CONFLICTING", "gh checks failed")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 2 * time.Second

	started := time.Unix(1700000000, 0)
	now := started
	step := &CIStep{
		now: func() time.Time {
			return now
		},
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			now = now.Add(interval)
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval when merge conflict remains known but checks lookup fails")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}

	foundConflict := false
	for _, item := range findings.Items {
		if strings.Contains(strings.ToLower(item.Description), "merge conflict") {
			foundConflict = true
			break
		}
	}
	if !foundConflict {
		t.Fatalf("expected merge conflict finding, got %+v", findings.Items)
	}
}

func TestCIStep_WaitsForPendingChecksBeforeFixing(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Poll 1: build failing, test still pending -> should NOT fix yet
	// Poll 2: build failing, test also failing -> should fix now
	checksSequence := []string{
		`[{"name":"build","bucket":"fail"},{"name":"test","bucket":"pending"}]`,
		`[{"name":"build","bucket":"fail"},{"name":"test","bucket":"fail"}]`,
	}
	env := fakeCIGHSequenceMergeable(t, "OPEN", checksSequence, "MERGEABLE")

	agentCalled := false
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			agentCalled = true
			os.WriteFile(filepath.Join(opts.CWD, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount >= 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	_, err := step.Execute(sctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}

	// Agent should have been called on poll 2 (after pending resolved), not poll 1
	if !agentCalled {
		t.Fatal("expected agent to be called after all checks completed")
	}

	// Should have logged about waiting for pending checks
	foundWaiting := false
	for _, l := range logs {
		if strings.Contains(l, "waiting for") && strings.Contains(l, "pending") {
			foundWaiting = true
			break
		}
	}
	if !foundWaiting {
		t.Fatalf("expected log about waiting for pending checks, got: %v", logs)
	}
}
