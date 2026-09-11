package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// stubShellEnv replaces the daemon's login-shell probes with an in-memory
// model: degraded reports the current state, and refresh flips it to healthy
// while prepending marker to PATH the way a real probe would.
type stubShellEnv struct {
	degraded  bool
	refreshes int
	err       error
	marker    string
}

func (s *stubShellEnv) install(t *testing.T) {
	t.Helper()
	oldApply, oldRefresh, oldDegraded := applyShellEnvToProcess, refreshShellEnvToProcess, shellEnvDegraded
	t.Setenv("PATH", os.Getenv("PATH"))
	shellEnvDegraded = func() bool { return s.degraded }
	refreshShellEnvToProcess = func() error {
		s.refreshes++
		if s.err != nil {
			return s.err
		}
		s.degraded = false
		return os.Setenv("PATH", s.marker+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	t.Cleanup(func() {
		applyShellEnvToProcess, refreshShellEnvToProcess, shellEnvDegraded = oldApply, oldRefresh, oldDegraded
	})
}

func TestRefreshDegradedShellEnvironment_ReprobesOnlyWhileDegraded(t *testing.T) {
	stub := &stubShellEnv{degraded: true, marker: "/recovered/bin"}
	stub.install(t)
	t.Setenv("NM_HOME", "/service/root")
	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	refreshDegradedShellEnvironment()
	if stub.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1 while degraded", stub.refreshes)
	}
	if !strings.HasPrefix(os.Getenv("PATH"), "/recovered/bin") {
		t.Fatalf("expected the recovered PATH applied to the process, got %q", os.Getenv("PATH"))
	}
	if got := os.Getenv("NM_HOME"); got != "/service/root" {
		t.Fatalf("NM_HOME = %q, want the service value preserved across a refresh", got)
	}
	if !strings.Contains(buf.String(), "recovered from the degraded fallback") || !strings.Contains(buf.String(), "daemon environment ready") {
		t.Fatalf("expected recovery and PATH summary logs, got %q", buf.String())
	}

	refreshDegradedShellEnvironment()
	if stub.refreshes != 1 {
		t.Fatalf("refreshes = %d, want no re-probe once healthy", stub.refreshes)
	}
}

func TestConcurrentRefreshDegradedShellEnvironment_PreservesServiceNMHome(t *testing.T) {
	t.Setenv("NM_HOME", "/service/root")
	oldRefresh, oldDegraded := refreshShellEnvToProcess, shellEnvDegraded
	var degraded atomic.Bool
	degraded.Store(true)
	var calls atomic.Int32
	firstApplied := make(chan struct{})
	secondApplied := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	shellEnvDegraded = degraded.Load
	refreshShellEnvToProcess = func() error {
		switch calls.Add(1) {
		case 1:
			if err := os.Setenv("NM_HOME", "/shell/root"); err != nil {
				return err
			}
			close(firstApplied)
			<-releaseFirst
			degraded.Store(false)
			return nil
		case 2:
			close(secondApplied)
			<-releaseSecond
			return nil
		default:
			return errors.New("unexpected refresh")
		}
	}
	t.Cleanup(func() {
		refreshShellEnvToProcess, shellEnvDegraded = oldRefresh, oldDegraded
	})

	var wg sync.WaitGroup
	firstDone := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		refreshDegradedShellEnvironment()
		close(firstDone)
	}()
	<-firstApplied
	go func() {
		defer wg.Done()
		refreshDegradedShellEnvironment()
	}()

	select {
	case <-secondApplied:
		close(releaseFirst)
		<-firstDone
	case <-time.After(100 * time.Millisecond):
		close(releaseFirst)
		<-firstDone
	}
	close(releaseSecond)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("refreshes = %d, want one serialized successful refresh", got)
	}
	if got := os.Getenv("NM_HOME"); got != "/service/root" {
		t.Fatalf("NM_HOME = %q, want service value after concurrent refreshes", got)
	}
}

func TestRefreshDegradedShellEnvironment_KeepsRunningOnRefreshFailure(t *testing.T) {
	stub := &stubShellEnv{degraded: true, err: errors.New("probe exploded")}
	stub.install(t)
	before := os.Getenv("PATH")
	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	refreshDegradedShellEnvironment()
	if os.Getenv("PATH") != before {
		t.Fatalf("PATH changed on a failed refresh: %q", os.Getenv("PATH"))
	}
	if !strings.Contains(buf.String(), "login shell environment refresh failed") {
		t.Fatalf("expected a refresh failure warning, got %q", buf.String())
	}
}

type capturePathStep struct {
	seen chan<- string
}

func (s *capturePathStep) Name() types.StepName { return types.StepReview }
func (s *capturePathStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.seen <- os.Getenv("PATH")
	return &pipeline.StepOutcome{}, nil
}

// TestRunStartRecoversADegradedLoginShellEnvironment is the #143 lifetime
// pin: a daemon whose startup probe fell back must not run every pipeline on
// the degraded PATH until restart. The first run start re-probes, and its
// steps already see the recovered environment.
func TestRunStartRecoversADegradedLoginShellEnvironment(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	stub := &stubShellEnv{degraded: true, marker: "/etc/profiles/per-user/test/bin"}
	stub.install(t)
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "shellenv-recovery")

	seen := make(chan string, 2)
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&capturePathStep{seen: seen}}
	})
	t.Cleanup(manager.Shutdown)
	for i := 0; i < 2; i++ {
		runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "recover the login shell environment", "")
		if err != nil {
			t.Fatalf("start run %d: %v", i, err)
		}
		if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
			t.Fatalf("run %d status = %s, error = %v", i, run.Status, run.Error)
		}
		if got := <-seen; !strings.HasPrefix(got, stub.marker) {
			t.Fatalf("run %d step PATH = %q, want the recovered login shell PATH first", i, got)
		}
	}
	if stub.refreshes != 1 {
		t.Fatalf("refreshes = %d, want exactly one re-probe: the first run recovers, the second is already healthy", stub.refreshes)
	}
}
