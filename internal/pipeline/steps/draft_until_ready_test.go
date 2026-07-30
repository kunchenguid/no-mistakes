package steps

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// stubBaseBranchTip keeps the draft-lifecycle tests off the real git fetch the
// CI monitor otherwise runs on every poll: these tests assert the PR-publish
// transition, not timeout re-arming.
func stubBaseBranchTip(context.Context) (string, bool) { return "", false }

// readFakeCLILog returns the recorded fake-CLI argv log, or "" when the fake
// binary was never invoked.
func readFakeCLILog(t *testing.T, logFile string) string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

// countGHInvocations counts fake-CLI log lines whose argv starts with prefix.
func countGHInvocations(t *testing.T, logFile, prefix string) int {
	t.Helper()
	count := 0
	for _, line := range strings.Split(readFakeCLILog(t, logFile), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			count++
		}
	}
	return count
}

func TestPRStep_DraftUntilReadyOpensDraftPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.DraftUntilReady = true

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	ghLog := readFakeCLILog(t, logFile)
	if !strings.Contains(ghLog, "pr create") {
		t.Fatalf("expected gh pr create, got:\n%s", ghLog)
	}
	if !strings.Contains(ghLog, "--draft") {
		t.Fatalf("expected --draft on gh pr create when draft-until-ready is set, got:\n%s", ghLog)
	}
}

func TestPRStep_WithoutDraftUntilReadyOpensNormalPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	ghLog := readFakeCLILog(t, logFile)
	if !strings.Contains(ghLog, "pr create") {
		t.Fatalf("expected gh pr create, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--draft") {
		t.Fatalf("expected no --draft without the flag, got:\n%s", ghLog)
	}
}

// The update path adopts a PR whose draft state the pipeline does not own: a
// second run must never re-draft a PR reviewers are already looking at, and
// must never mark one ready ahead of the CI-green edge.
func TestPRStep_DraftUntilReadyNeverTouchesExistingPRDraftState(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.DraftUntilReady = true

	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	ghLog := readFakeCLILog(t, logFile)
	if !strings.Contains(ghLog, "pr edit") {
		t.Fatalf("expected gh pr edit on the update path, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "--draft") {
		t.Fatalf("update path must not emit draft args, got:\n%s", ghLog)
	}
	if strings.Contains(ghLog, "pr ready") {
		t.Fatalf("PR step must never mark a PR ready, got:\n%s", ghLog)
	}
}

// Bitbucket Cloud has no draft pull requests at all. The run must open the PR
// normally and say why, never fail.
func TestPRStep_DraftUntilReadyOnProviderWithoutDraftLogsSkipAndOpensNormally(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	api := newFakeBitbucketPRAPI(t, 0, "")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeBitbucketEnv(api.server.URL)
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Run.DraftUntilReady = true

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &PRStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("draft on a provider without draft support must not fail the run: %v", err)
	}
	if outcome.Skipped {
		t.Fatal("expected the PR to still be created")
	}
	if api.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", api.createCalls)
	}

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "draft") || !strings.Contains(joined, "opening normally") {
		t.Fatalf("expected a draft-unsupported skip log, got:\n%s", joined)
	}
}

func TestCIStep_DraftUntilReadyMarksPRReadyOnceAtCIGreenEdge(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Pending, then green, then green again: the ready transition happens once.
	checksSequence := []string{
		`[{"name":"build","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
	}
	env, logFile := fakeCIGHSequenceLogged(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Run.DraftUntilReady = true
	sctx.Config.CITimeout = time.Hour

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		baseBranchTip: stubBaseBranchTip,
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
			polls++
			if polls < 3 {
				return nil
			}
			cancel()
			return ctx.Err()
		},
	}
	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue until cancelled, got %v", err)
	}

	if got := countGHInvocations(t, logFile, "pr ready"); got != 1 {
		t.Fatalf("gh pr ready invocations = %d, want exactly 1\nlog:\n%s", got, readFakeCLILog(t, logFile))
	}
	if !strings.Contains(readFakeCLILog(t, logFile), "pr ready 42") {
		t.Fatalf("expected an explicit PR selector on gh pr ready, got:\n%s", readFakeCLILog(t, logFile))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "ready for review") {
		t.Fatalf("expected a ready-for-review log line, got:\n%s", strings.Join(logs, "\n"))
	}
}

// A repo that positively declares `no_ci: true` on its trusted default branch
// still reaches "everything verifiable is green" with zero checks, so that
// declared-no-CI edge publishes the draft.
func TestCIStep_DraftUntilReadyPublishesOnTrustedNoCI(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeCIGHSequenceLogged(t, "OPEN", []string{`[]`, `[]`})

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Run.DraftUntilReady = true
	sctx.Config.CITimeout = time.Hour
	sctx.Config.NoCI = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		baseBranchTip: stubBaseBranchTip,
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
			polls++
			if polls < 2 {
				return nil
			}
			cancel()
			return ctx.Err()
		},
	}
	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue until cancelled, got %v", err)
	}

	if got := countGHInvocations(t, logFile, "pr ready"); got != 1 {
		t.Fatalf("gh pr ready invocations = %d, want exactly 1\nlog:\n%s", got, readFakeCLILog(t, logFile))
	}
}

// An empty forge check list without a trusted `no_ci: true` declaration is
// unproven, not green: delayed check registration is common, and a repo that
// gates its workflows on `draft == false` reports zero checks precisely BECAUSE
// the PR is still a draft. Publishing there would be self-fulfilling.
func TestCIStep_DraftUntilReadyKeepsDraftForUnprovenEmptyChecks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeCIGHSequenceLogged(t, "OPEN", []string{`[]`, `[]`, `[]`})

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Run.DraftUntilReady = true
	sctx.Config.CITimeout = time.Hour
	sctx.Config.NoCI = false

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		baseBranchTip: stubBaseBranchTip,
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
			polls++
			if polls < 3 {
				return nil
			}
			cancel()
			return ctx.Err()
		},
	}
	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue until cancelled, got %v", err)
	}

	if got := countGHInvocations(t, logFile, "pr ready"); got != 0 {
		t.Fatalf("gh pr ready invocations = %d, want 0 for unproven empty checks\nlog:\n%s", got, readFakeCLILog(t, logFile))
	}
}

func TestCIStep_WithoutDraftUntilReadyNeverMarksPRReady(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeCIGHSequenceLogged(t, "OPEN", []string{`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`})

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		baseBranchTip: stubBaseBranchTip,
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue until cancelled, got %v", err)
	}

	if got := countGHInvocations(t, logFile, "pr ready"); got != 0 {
		t.Fatalf("gh pr ready invocations = %d, want 0 without the flag\nlog:\n%s", got, readFakeCLILog(t, logFile))
	}
}

// Ready is one-way: a PR that goes green, red, then green again is marked ready
// exactly once and is never converted back to a draft.
func TestCIStep_DraftUntilReadyIsOneWayAcrossACIRegression(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksSequence := []string{
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
		`[{"name":"build","state":"FAILURE","bucket":"fail"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
	}
	env, logFile := fakeCIGHSequenceLogged(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Run.DraftUntilReady = true
	sctx.Config.CITimeout = time.Hour
	sctx.Config.AutoFix.CI = 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		baseBranchTip: stubBaseBranchTip,
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
			polls++
			if polls < 3 {
				return nil
			}
			cancel()
			return ctx.Err()
		},
	}
	// Auto-fix is disabled, so the failing round returns a manual-intervention
	// outcome; either way the ready transition must have happened exactly once
	// and no un-draft may follow it.
	_, _ = step.Execute(sctx)

	ghLog := readFakeCLILog(t, logFile)
	if got := countGHInvocations(t, logFile, "pr ready"); got != 1 {
		t.Fatalf("gh pr ready invocations = %d, want exactly 1\nlog:\n%s", got, ghLog)
	}
	if strings.Contains(ghLog, "--undo") {
		t.Fatalf("a red CI run must never convert the PR back to a draft, got:\n%s", ghLog)
	}
}

// The whole point of the flag is to defer reviewer tagging until CI is green,
// so a PR the pipeline never saw go green must stay a draft.
func TestCIStep_DraftUntilReadyLeavesRedPRAsDraft(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeCIGHSequenceLogged(t, "OPEN", []string{`[{"name":"build","state":"FAILURE","bucket":"fail"}]`})

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Run.DraftUntilReady = true
	sctx.Config.CITimeout = time.Hour
	sctx.Config.AutoFix.CI = 0

	step := &CIStep{baseBranchTip: stubBaseBranchTip}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("expected the failing-CI gate, got %+v", outcome)
	}
	if got := countGHInvocations(t, logFile, "pr ready"); got != 0 {
		t.Fatalf("gh pr ready invocations = %d, want 0 while CI is red\nlog:\n%s", got, readFakeCLILog(t, logFile))
	}
}

// A rerun adopts the PR a previous run already published. Publishing it again
// is pointless, so the CI step skips it once the PR step has reported the
// adopted PR as non-draft.
func TestCIStep_DraftUntilReadySkipsPublishForAnAlreadyPublishedPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, logFile := fakeCIGHSequenceLogged(t, "OPEN", []string{`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`})

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Run.DraftUntilReady = true
	sctx.Config.CITimeout = time.Hour
	sctx.Shared = &pipeline.RunShared{}
	sctx.Shared.SetPRDraftState(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := &CIStep{
		baseBranchTip: stubBaseBranchTip,
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected monitoring to continue until cancelled, got %v", err)
	}

	if got := countGHInvocations(t, logFile, "pr ready"); got != 0 {
		t.Fatalf("gh pr ready invocations = %d, want 0 for an already-published PR\nlog:\n%s", got, readFakeCLILog(t, logFile))
	}
}

// The PR step is the only place that observes draft state, so it must hand it
// to the CI step for both the created and the adopted PR.
func TestPRStep_RecordsPRDraftStateForTheCIStep(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		prViewURL string
		flag      bool
		wantDraft bool
	}{
		{"creates a draft", "", true, true},
		{"creates a normal PR", "", false, false},
		// The fake gh reports the adopted PR without isDraft, i.e. published.
		{"adopts a published PR", "https://github.com/test/repo/pull/42", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			env, _ := fakeGH(t, tc.prViewURL)

			ag := &mockAgent{name: "test"}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = env
			sctx.Run.DraftUntilReady = tc.flag
			sctx.Shared = &pipeline.RunShared{}

			step := &PRStep{}
			if _, err := step.Execute(sctx); err != nil {
				t.Fatal(err)
			}

			isDraft, known := sctx.Shared.PRDraftState()
			if !known {
				t.Fatal("PR step did not record the PR's draft state")
			}
			if isDraft != tc.wantDraft {
				t.Fatalf("recorded draft state = %v, want %v", isDraft, tc.wantDraft)
			}
		})
	}
}

func TestDraftUntilReadyPersistsOnTheRun(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.DraftUntilReady {
		t.Fatal("draft_until_ready must default to false")
	}

	if err := sctx.DB.SetRunDraftUntilReady(sctx.Run.ID, true); err != nil {
		t.Fatal(err)
	}
	run, err = sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.DraftUntilReady {
		t.Fatal("draft_until_ready did not survive the round trip")
	}
}
