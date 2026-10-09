package github

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const secondaryLimitStderr = "gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (HTTP 403)"

func TestSecondaryRateLimitWait(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		output   string
		wantOK   bool
		wantWait time.Duration
	}{
		{"rest secondary limit", secondaryLimitStderr, true, 0},
		{"graphql already exceeded", "GraphQL: API rate limit already exceeded for user ID 229287877.", true, 0},
		{"abuse detection", "You have triggered an abuse detection mechanism", true, 0},
		{"retry-after header", "HTTP 403\nRetry-After: 17", true, 17 * time.Second},
		{"retry after sentence", "secondary rate limit, retry after 9 seconds", true, 9 * time.Second},
		{"permission 403 is not a limit", "gh: Resource not accessible by integration (HTTP 403)", false, 0},
		{"plain 429 without a marker", "HTTP 429", false, 0},
		{"not found", "GraphQL: Could not resolve to a PullRequest (HTTP 404)", false, 0},
		{"empty", "", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wait, ok := secondaryRateLimitWait(tc.output)
			if ok != tc.wantOK || wait != tc.wantWait {
				t.Fatalf("secondaryRateLimitWait(%q) = (%v, %v), want (%v, %v)", tc.output, wait, ok, tc.wantWait, tc.wantOK)
			}
		})
	}
}

func TestRateLimitBackoffStaysWithinJitterAndCap(t *testing.T) {
	t.Parallel()

	for retry := 0; retry < 8; retry++ {
		full := rateLimitBackoffBase << retry
		if full > rateLimitMaxWait {
			full = rateLimitMaxWait
		}
		for _, unit := range []float64{0, 0.25, 0.5, 0.999999} {
			got := rateLimitBackoff(retry, unit)
			if got < full/2 || got > full {
				t.Fatalf("rateLimitBackoff(%d, %v) = %v, want within [%v, %v]", retry, unit, got, full/2, full)
			}
			if got > rateLimitMaxWait {
				t.Fatalf("rateLimitBackoff(%d, %v) = %v exceeds the cap %v", retry, unit, got, rateLimitMaxWait)
			}
		}
	}
	if lo, hi := rateLimitBackoff(0, 0), rateLimitBackoff(0, 0.999999); lo >= hi {
		t.Fatalf("jitter does not spread the wait: %v vs %v", lo, hi)
	}
}

// sequenceFactory answers each gh invocation with the next response in seq
// (the last one repeats) and records every call's command line.
type sequenceFactory struct {
	mu    sync.Mutex
	seq   []githubTestResponse
	calls []string
}

func (f *sequenceFactory) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	resp := f.seq[min(len(f.calls), len(f.seq)-1)]
	f.calls = append(f.calls, key)
	return githubTestCmdFactory(map[string]githubTestResponse{key: resp})(ctx, name, args...)
}

func (f *sequenceFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newRetryHost(f *sequenceFactory, waits *[]time.Duration) *Host {
	h := New(f.cmd, nil, "", "test/repo")
	h.rateLimitSleep = func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
	return h
}

func TestFindPRRetriesSecondaryRateLimit(t *testing.T) {
	t.Parallel()

	f := &sequenceFactory{seq: []githubTestResponse{
		{stderr: secondaryLimitStderr, code: 1},
		{stderr: "GraphQL: API rate limit already exceeded for user ID 1.", code: 1},
		{stdout: `[{"number":7,"url":"https://github.com/test/repo/pull/7","baseRefName":"main"}]`},
	}}
	var waits []time.Duration
	h := newRetryHost(f, &waits)

	pr, err := h.FindPR(context.Background(), "feature", "")
	if err != nil {
		t.Fatalf("FindPR() error = %v, want the retry to recover", err)
	}
	if pr == nil || pr.Number != "7" {
		t.Fatalf("FindPR() = %+v, want PR 7", pr)
	}
	if f.count() != 3 {
		t.Fatalf("gh ran %d times, want 3", f.count())
	}
	if len(waits) != 2 {
		t.Fatalf("waited %d times, want 2 (%v)", len(waits), waits)
	}
	for i, w := range waits {
		full := rateLimitBackoffBase << i
		if w < full/2 || w > full {
			t.Fatalf("wait %d = %v, want within [%v, %v]", i, w, full/2, full)
		}
	}
}

func TestRunReadHonoursRetryAfter(t *testing.T) {
	t.Parallel()

	f := &sequenceFactory{seq: []githubTestResponse{
		{stderr: "HTTP 429\nRetry-After: 7", code: 1},
		{stdout: "ok"},
	}}
	var waits []time.Duration
	h := newRetryHost(f, &waits)

	out, _, err := h.runRead(context.Background(), false, "pr", "view", "1")
	if err != nil || string(out) != "ok" {
		t.Fatalf("runRead() = (%q, %v), want ok", out, err)
	}
	if len(waits) != 1 || waits[0] != 7*time.Second {
		t.Fatalf("waits = %v, want exactly the 7s Retry-After", waits)
	}
}

func TestRunReadDoesNotRetryAboveTheWaitCap(t *testing.T) {
	t.Parallel()

	f := &sequenceFactory{seq: []githubTestResponse{
		{stderr: "HTTP 403\nRetry-After: 3600", code: 1},
		{stdout: "ok"},
	}}
	var waits []time.Duration
	h := newRetryHost(f, &waits)

	if _, _, err := h.runRead(context.Background(), false, "pr", "view", "1"); err == nil {
		t.Fatal("runRead() error = nil, want the refusal surfaced")
	}
	if f.count() != 1 || len(waits) != 0 {
		t.Fatalf("calls=%d waits=%v, want one call and no wait", f.count(), waits)
	}
}

func TestRunReadIsBounded(t *testing.T) {
	t.Parallel()

	f := &sequenceFactory{seq: []githubTestResponse{{stderr: secondaryLimitStderr, code: 1}}}
	var waits []time.Duration
	h := newRetryHost(f, &waits)

	_, detail, err := h.runRead(context.Background(), true, "api", "graphql")
	if err == nil {
		t.Fatal("runRead() error = nil, want the final refusal")
	}
	if !strings.Contains(detail, "secondary rate limit") {
		t.Fatalf("detail = %q, want the refusal text", detail)
	}
	if f.count() != rateLimitMaxAttempts {
		t.Fatalf("gh ran %d times, want %d", f.count(), rateLimitMaxAttempts)
	}
	if len(waits) != rateLimitMaxAttempts-1 {
		t.Fatalf("waits = %v, want %d", waits, rateLimitMaxAttempts-1)
	}
}

func TestRunReadDoesNotRetryOtherFailures(t *testing.T) {
	t.Parallel()

	f := &sequenceFactory{seq: []githubTestResponse{{stderr: "gh: Resource not accessible by integration (HTTP 403)", code: 1}}}
	var waits []time.Duration
	h := newRetryHost(f, &waits)

	if _, _, err := h.runRead(context.Background(), false, "pr", "view", "1"); err == nil {
		t.Fatal("runRead() error = nil, want failure")
	}
	if f.count() != 1 || len(waits) != 0 {
		t.Fatalf("calls=%d waits=%v, want a single call", f.count(), waits)
	}
}

func TestRunReadStopsWaitingWhenContextEnds(t *testing.T) {
	t.Parallel()

	f := &sequenceFactory{seq: []githubTestResponse{{stderr: secondaryLimitStderr, code: 1}}}
	h := New(f.cmd, nil, "", "test/repo")
	h.rateLimitSleep = func(context.Context, time.Duration) error { return context.Canceled }

	_, _, err := h.runRead(context.Background(), false, "pr", "view", "1")
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("runRead() error = %v, want the underlying refusal", err)
	}
	if f.count() != 1 {
		t.Fatalf("gh ran %d times, want 1", f.count())
	}
}

// One poll reads state, mergeability and head with a single `gh pr view`.
func TestGetPRPollSnapshotIsOneRequest(t *testing.T) {
	t.Parallel()

	f := &sequenceFactory{seq: []githubTestResponse{
		{stdout: `{"state":"OPEN","mergeable":"CONFLICTING","headRefOid":"deadbeef"}` + "\n"},
	}}
	var waits []time.Duration
	h := newRetryHost(f, &waits)

	var _ scm.PRPollSnapshotter = h
	snap, err := h.GetPRPollSnapshot(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatalf("GetPRPollSnapshot() error = %v", err)
	}
	want := scm.PRPollSnapshot{State: scm.PRStateOpen, Mergeable: scm.MergeableConflict, HeadSHA: "deadbeef"}
	if snap != want {
		t.Fatalf("snapshot = %+v, want %+v", snap, want)
	}
	if f.count() != 1 {
		t.Fatalf("gh ran %d times, want exactly 1", f.count())
	}
	const wantCmd = "gh pr view 123 --repo test/repo --json state,mergeable,headRefOid"
	if f.calls[0] != wantCmd {
		t.Fatalf("command = %q, want %q", f.calls[0], wantCmd)
	}
}

// With the poll's head supplied, check discovery makes no `gh pr view` at all.
func TestGetChecksUsesPolledHeadWithoutReReadingThePR(t *testing.T) {
	t.Parallel()

	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		githubCommitChecksCommand("", "test/repo", "deadbeef"): {
			stdout: githubCommitChecksResponse(`[
				{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-08-26T08:25:50Z","completedAt":"2026-08-26T08:25:56Z","detailsUrl":"https://github.com/test/repo/actions/runs/101/job/201"}
			]`),
		},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=deadbeef -f per_page=100 --paginate --slurp": {
			stdout: `[{"total_count":1,"workflow_runs":[
				{"id":101,"workflow_id":1001,"name":"build","status":"completed","conclusion":"success","run_started_at":"2026-08-26T08:25:50Z"}
			]}]` + "\n",
		},
	}), nil, "", "test/repo")

	pr := &scm.PR{Number: "123", HeadSHA: "stale", PollHeadSHA: "deadbeef"}
	checks, err := host.GetChecks(context.Background(), pr)
	if err != nil {
		t.Fatalf("GetChecks() error = %v (an unexpected gh pr view would surface here)", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" {
		t.Fatalf("checks = %+v, want the build check", checks)
	}
	if pr.HeadSHA != "deadbeef" {
		t.Fatalf("pr.HeadSHA = %q, want the polled head", pr.HeadSHA)
	}
}
