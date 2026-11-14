package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// publishingPushStep pushes the run head to the repository's upstream and
// records the publication, the way the real Push step's verified push does.
type publishingPushStep struct {
	execCnt atomic.Int32
}

func (s *publishingPushStep) Name() types.StepName { return types.StepPush }

func (s *publishingPushStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.execCnt.Add(1)
	pushURL := sctx.Repo.PushURL()
	ref := "refs/heads/" + sctx.Run.Branch
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "push", pushURL, "HEAD:"+ref); err != nil {
		return nil, err
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{
		HeadSHA: head, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(pushURL), Ref: ref,
	})
}

// flakyDeliveryStep fails its first execution the way a forge outage does and
// passes every later one.
type flakyDeliveryStep struct {
	name    types.StepName
	execCnt atomic.Int32
}

func (s *flakyDeliveryStep) Name() types.StepName { return s.name }

func (s *flakyDeliveryStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if s.execCnt.Add(1) == 1 {
		return nil, errors.New("gh: GraphQL: Bad Gateway (HTTP 502)")
	}
	return &pipeline.StepOutcome{}, nil
}

type deliveryFixture struct {
	p        *paths.Paths
	d        *db.DB
	repo     *db.Repo
	upstream string
	head     string
	source   *db.Run
	test     *mockPassStep
	push     *publishingPushStep
	pr       pipeline.Step
	ci       pipeline.Step
}

// newDeliveryFixture runs a pipeline whose failing step fails once after the
// push, so the run it returns failed in its delivery with its head published.
func newDeliveryFixture(t *testing.T, failing types.StepName) *deliveryFixture {
	t.Helper()
	f := &deliveryFixture{test: &mockPassStep{name: types.StepTest}, push: &publishingPushStep{}}
	f.pr, f.ci = &mockPassStep{name: types.StepPR}, &mockPassStep{name: types.StepCI}
	if failing == types.StepPR {
		f.pr = &flakyDeliveryStep{name: types.StepPR}
	} else {
		f.ci = &flakyDeliveryStep{name: types.StepCI}
	}
	f.p, f.d = startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{f.test, f.push, f.pr, f.ci} })
	f.repo, f.head = setupTestGitRepo(t, f.p, f.d, "delivery-retry-repo")
	f.upstream = filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, "", "init", "--bare", f.upstream)
	repo, err := f.d.ReplaceRepoURLs(f.repo.ID, f.upstream, "")
	if err != nil {
		t.Fatal(err)
	}
	f.repo = repo

	client, err := ipc.Dial(f.p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: f.p.RepoDir(f.repo.ID), Ref: "refs/heads/main", New: f.head, Old: f.head, Intent: "deliver the change",
	}, &first); err != nil {
		t.Fatal(err)
	}
	f.source = waitForRunTerminalState(t, f.d, first.RunID)
	if f.source.Status != types.RunFailed || f.source.LastPushedSHA == nil || *f.source.LastPushedSHA != f.head {
		t.Fatalf("first run = %+v, want a run that pushed %s and then failed", f.source, f.head)
	}
	if got := gitOutput(t, f.upstream, "rev-parse", "refs/heads/main"); got != f.head {
		t.Fatalf("upstream main = %s, want the pushed head %s", got, f.head)
	}
	return f
}

func (f *deliveryFixture) retry(t *testing.T) (ipc.RetryRunResult, error) {
	t.Helper()
	client, err := ipc.Dial(f.p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RetryRunResult
	err = client.Call(ipc.MethodRetryRun, &ipc.RetryRunParams{RepoID: f.repo.ID, Branch: "main", RunID: f.source.ID, CallerHeadSHA: f.head}, &result)
	return result, err
}

// moveRemoteBranch pushes a commit the run never saw to the remote branch.
func (f *deliveryFixture) moveRemoteBranch(t *testing.T) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	gitCmd(t, "", "clone", "-b", "main", f.upstream, other)
	gitCmd(t, other, "config", "user.email", "test@test.com")
	gitCmd(t, other, "config", "user.name", "Test")
	gitCmd(t, other, "commit", "--allow-empty", "-m", "out of band")
	gitCmd(t, other, "push", "origin", "HEAD:refs/heads/main")
}

func execCount(step pipeline.Step) int32 {
	switch s := step.(type) {
	case *mockPassStep:
		return s.execCnt.Load()
	case *flakyDeliveryStep:
		return s.execCnt.Load()
	}
	return -1
}

func TestRetryContinuesOnlyTheDeliveryOfAPushedRun(t *testing.T) {
	for _, failing := range []types.StepName{types.StepPR, types.StepCI} {
		t.Run(string(failing), func(t *testing.T) {
			f := newDeliveryFixture(t, failing)
			result, err := f.retry(t)
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if result.Step != failing || result.SourceRunID != f.source.ID {
				t.Fatalf("retry = %+v, want a continuation of %s at %s", result, f.source.ID, failing)
			}
			continued := waitForRunTerminalState(t, f.d, result.RunID)
			if continued.Status != types.RunCompleted {
				t.Fatalf("continuation = %s (%v), want completed", continued.Status, continued.Error)
			}
			if got := f.test.execCnt.Load(); got != 1 {
				t.Errorf("test executed %d times, want once: the continuation carries it", got)
			}
			if got := f.push.execCnt.Load(); got != 1 {
				t.Errorf("push executed %d times, want once: the continuation never pushes", got)
			}
			wantPR, wantCI := int32(1), int32(2)
			if failing == types.StepPR {
				wantPR, wantCI = 2, 1
			}
			if got := execCount(f.pr); got != wantPR {
				t.Errorf("pr executed %d times, want %d", got, wantPR)
			}
			if got := execCount(f.ci); got != wantCI {
				t.Errorf("ci executed %d times, want %d", got, wantCI)
			}
			if continued.LastPushedSHA == nil || *continued.LastPushedSHA != f.head || continued.PushTargetFingerprint == nil || *continued.PushTargetFingerprint != *f.source.PushTargetFingerprint {
				t.Fatalf("continuation push binding = %v/%v, want the verified head %s on the source's target", continued.LastPushedSHA, continued.PushTargetFingerprint, f.head)
			}
			if continued.EvidencePath() == "" || continued.EvidencePath() != f.source.EvidencePath() {
				t.Errorf("continuation evidence = %q, want the source run's %q", continued.EvidencePath(), f.source.EvidencePath())
			}
			rows, err := f.d.GetStepsByRun(continued.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				carried := row.CarriedFrom != nil
				if want := row.StepName.Order() < failing.Order(); carried != want {
					t.Errorf("%s carried = %v, want %v", row.StepName, carried, want)
				}
			}
		})
	}
}

func TestRetryRefusesADeliveryContinuationWithoutAFreshMatchingRemoteHead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, f *deliveryFixture)
		reason string
	}{
		{name: "remote branch moved", reason: "not the run's pushed head", change: func(t *testing.T, f *deliveryFixture) { f.moveRemoteBranch(t) }},
		{name: "remote branch deleted", reason: "is absent", change: func(t *testing.T, f *deliveryFixture) {
			gitCmd(t, f.upstream, "update-ref", "-d", "refs/heads/main")
		}},
		{name: "remote unreadable", reason: "could not be read", change: func(t *testing.T, f *deliveryFixture) {
			if err := os.Rename(f.upstream, f.upstream+".gone"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "push target changed", reason: "push target changed", change: func(t *testing.T, f *deliveryFixture) {
			if _, err := f.d.ReplaceRepoURLs(f.repo.ID, f.upstream+".elsewhere", ""); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t, types.StepPR)
			tc.change(t, f)
			result, err := f.retry(t)
			if err == nil {
				t.Fatalf("retry started %s, want a published head refusal", result.RunID)
			}
			for _, want := range []string{"published head proof failed", tc.reason, "no-mistakes rerun"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
			runs, err := f.d.GetRunsByRepo(f.repo.ID)
			if err != nil || len(runs) != 1 {
				t.Fatalf("refusal created a run: count=%d err=%v", len(runs), err)
			}
			if got := f.push.execCnt.Load(); got != 1 {
				t.Errorf("push executed %d times, want only the source run's push", got)
			}
		})
	}
}

func TestRetryReprovesThePublishedHeadAtLaunchAndKeepsTheSourceEvidence(t *testing.T) {
	f := newDeliveryFixture(t, types.StepPR)
	artifact := filepath.Join(f.source.EvidencePath(), "screenshot.png")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(f.d, f.p, func() []pipeline.Step { return []pipeline.Step{f.test, f.push, f.pr, f.ci} })
	t.Cleanup(m.Shutdown)
	m.afterRetryProof = func() { f.moveRemoteBranch(t) }

	_, err := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.head)
	if err == nil || !strings.Contains(err.Error(), "published head proof failed") || !strings.Contains(err.Error(), "not the run's pushed head") {
		t.Fatalf("retry = %v, want a launch-time published head refusal", err)
	}
	if runs, err := f.d.GetRunsByRepo(f.repo.ID); err != nil || len(runs) != 1 {
		t.Fatalf("refused launch left %d runs (err %v), want only the source", len(runs), err)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("refused launch removed the source run's evidence: %v", err)
	}
	if got := f.push.execCnt.Load(); got != 1 {
		t.Errorf("push executed %d times, want only the source run's push", got)
	}
}
