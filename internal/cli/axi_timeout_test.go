package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiWaitFlagDefaultIsEightMinutes(t *testing.T) {
	runWait, err := newAxiRunCmd().Flags().GetDuration("wait")
	if err != nil {
		t.Fatal(err)
	}
	respondWait, err := newAxiRespondCmd().Flags().GetDuration("wait")
	if err != nil {
		t.Fatal(err)
	}
	if runWait != 8*time.Minute || respondWait != 8*time.Minute {
		t.Fatalf("run=%s respond=%s, want 8m on both", runWait, respondWait)
	}
}

func TestDriveRun_SlowGetRunRetriesAfterHealthProbe(t *testing.T) {
	setDriveGetRunTimeout(t, 60*time.Millisecond)

	var getRunCalls atomic.Int32
	var healthCalls atomic.Int32
	socketPath := filepath.Join(makeSocketSafeTempDir(t), "slow-get-run.sock")
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		healthCalls.Add(1)
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGetRun, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		n := getRunCalls.Add(1)
		if n == 1 {
			if err := sleepOrDone(ctx, 180*time.Millisecond); err != nil {
				return nil, err
			}
		}
		return &ipc.GetRunResult{Run: &ipc.RunInfo{ID: "run-1", Status: types.RunCompleted}}, nil
	})
	srv.HandleStream(ipc.MethodSubscribe, hangSubscribe)
	startIPCServer(t, srv, socketPath)

	client := dialReady(t, socketPath)
	defer client.Close()

	started := time.Now()
	run, _, err := driveRun(context.Background(), io.Discard, client, socketPath, "run-1", false)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("slow live daemon treated as failure: %v", err)
	}
	if run == nil || run.Status != types.RunCompleted {
		t.Fatalf("run = %+v, want completed", run)
	}
	if getRunCalls.Load() < 2 {
		t.Fatalf("get_run calls = %d, want a timed-out attempt then a retry", getRunCalls.Load())
	}
	if healthCalls.Load() < 1 {
		t.Fatal("slow get_run did not health-probe before retrying")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("retry took %s, want prompt recovery", elapsed)
	}
}

func TestDriveRun_GetRunRPCErrorIsNotRetried(t *testing.T) {
	setDriveGetRunTimeout(t, 60*time.Millisecond)

	var healthCalls atomic.Int32
	socketPath := filepath.Join(makeSocketSafeTempDir(t), "rpc-fail.sock")
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		healthCalls.Add(1)
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return nil, errors.New("database unavailable")
	})
	srv.HandleStream(ipc.MethodSubscribe, hangSubscribe)
	startIPCServer(t, srv, socketPath)

	client := dialReady(t, socketPath)
	defer client.Close()

	_, _, err := driveRun(context.Background(), io.Discard, client, socketPath, "run-1", false)
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("error = %v, want genuine RPC failure", err)
	}
	if healthCalls.Load() != 0 {
		t.Fatalf("health probes = %d, genuine RPC failure must not take the slow-reply retry path", healthCalls.Load())
	}
}

func TestDriveRun_SlowGetRunWithFailedHealthIsDead(t *testing.T) {
	setDriveGetRunTimeout(t, 50*time.Millisecond)

	socketPath := filepath.Join(makeSocketSafeTempDir(t), "dead-after-slow.sock")
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return nil, errors.New("health unavailable")
	})
	srv.Handle(ipc.MethodGetRun, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		if err := sleepOrDone(ctx, 200*time.Millisecond); err != nil {
			return nil, err
		}
		return &ipc.GetRunResult{Run: &ipc.RunInfo{ID: "run-1", Status: types.RunRunning}}, nil
	})
	srv.HandleStream(ipc.MethodSubscribe, hangSubscribe)
	startIPCServer(t, srv, socketPath)

	client := dialReady(t, socketPath)
	defer client.Close()

	_, _, err := driveRun(context.Background(), io.Discard, client, socketPath, "run-1", false)
	if err == nil || !strings.Contains(err.Error(), "health probe failed") {
		t.Fatalf("error = %v, want health-probe failure after slow get_run", err)
	}
}

func TestAxiRun_WaitElapsedAgainstLiveIdleDaemon(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		return fx.running(), nil
	})

	started := time.Now()
	out, err := executeCmd("axi", "run", "--wait", "250ms")
	elapsed := time.Since(started)
	assertWaitElapsed(t, err, out, "250ms")
	if elapsed > 3*time.Second {
		t.Fatalf("bounded wait took %s, want return near 250ms", elapsed)
	}
}

func TestAxiRespond_WaitElapsedAgainstLiveIdleDaemon(t *testing.T) {
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{responded: &responded})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.running(), nil
		}
		return fx.awaiting(), nil
	})

	started := time.Now()
	out, err := executeCmd("axi", "respond", "--action", "approve", "--wait", "250ms")
	elapsed := time.Since(started)
	assertWaitElapsed(t, err, out, "250ms")
	if elapsed > 3*time.Second {
		t.Fatalf("bounded wait took %s, want return near 250ms", elapsed)
	}
}

func TestAxiRun_WaitZeroIsUsageError(t *testing.T) {
	out, err := executeCmd("axi", "run", "--wait", "0", "--intent", "x")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("wait 0 error = %v, want usage exit 2\n%s", err, out)
	}
	if !strings.Contains(out, "--wait must be a positive duration") {
		t.Fatalf("usage output missing wait error:\n%s", out)
	}
}

func TestAxiRun_SlowDaemonThenCompletesViaCLI(t *testing.T) {
	setDriveGetRunTimeout(t, 60*time.Millisecond)
	var getRunCalls atomic.Int32
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	fx.setGetRun(func(ctx context.Context, n int) (*ipc.RunInfo, error) {
		getRunCalls.Store(int32(n))
		if n == 1 {
			if err := sleepOrDone(ctx, 180*time.Millisecond); err != nil {
				return nil, err
			}
		}
		return fx.completed(), nil
	})

	out, err := executeCmd("axi", "run", "--wait", "5s")
	if err != nil {
		t.Fatalf("slow live daemon via axi run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "outcome: passed") {
		t.Fatalf("expected completed run, got:\n%s", out)
	}
	if getRunCalls.Load() < 2 {
		t.Fatalf("get_run calls = %d, want retry after slow first reply", getRunCalls.Load())
	}
}

func TestNoMistakesBinary_WaitAndSlowDaemon(t *testing.T) {
	bin := buildNoMistakesBinary(t)

	help := execNoMistakes(t, bin, "axi", "run", "--help")
	if !strings.Contains(help, "--wait") || !(strings.Contains(help, "8m0s") || strings.Contains(help, "8m")) {
		t.Fatalf("real binary axi run --help missing default --wait 8m:\n%s", help)
	}
	respondHelp := execNoMistakes(t, bin, "axi", "respond", "--help")
	if !strings.Contains(respondHelp, "--wait") {
		t.Fatalf("real binary axi respond --help missing --wait:\n%s", respondHelp)
	}

	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		return fx.running(), nil
	})
	started := time.Now()
	out, err := execNoMistakesErr(t, bin, "axi", "run", "--wait", "300ms")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatalf("expected wait elapsed from real binary, got:\n%s", out)
	}
	if !strings.Contains(out, "wait of 300ms elapsed") {
		t.Fatalf("real binary missing wait-elapsed error:\n%s", out)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("real binary wait took %s", elapsed)
	}

	fx = newAxiTimeoutFixture(t, axiTimeoutOpts{})
	var getRunCalls atomic.Int32
	fx.setGetRun(func(ctx context.Context, n int) (*ipc.RunInfo, error) {
		getRunCalls.Store(int32(n))
		if n == 1 {
			if err := sleepOrDone(ctx, 180*time.Millisecond); err != nil {
				return nil, err
			}
		}
		return fx.completed(), nil
	})
	out, err = execNoMistakesErr(t, bin, "axi", "run", "--wait", "5s")
	if err != nil {
		t.Fatalf("real binary slow-but-under-deadline daemon: %v\n%s", err, out)
	}
	if !strings.Contains(out, "outcome: passed") {
		t.Fatalf("real binary expected completed run:\n%s", out)
	}
}

type axiTimeoutOpts struct {
	responded *atomic.Bool
}

type axiTimeoutFixture struct {
	head   string
	mu     sync.Mutex
	getRun func(context.Context, int) (*ipc.RunInfo, error)
}

func (fx *axiTimeoutFixture) setGetRun(fn func(context.Context, int) (*ipc.RunInfo, error)) {
	fx.mu.Lock()
	fx.getRun = fn
	fx.mu.Unlock()
}

func (fx *axiTimeoutFixture) callGetRun(ctx context.Context, n int) (*ipc.RunInfo, error) {
	fx.mu.Lock()
	fn := fx.getRun
	fx.mu.Unlock()
	if fn == nil {
		return fx.running(), nil
	}
	return fn(ctx, n)
}

func (fx *axiTimeoutFixture) running() *ipc.RunInfo {
	return &ipc.RunInfo{
		ID:      "run-timeout",
		Branch:  "feature/timeout",
		Status:  types.RunRunning,
		HeadSHA: fx.head,
	}
}

func (fx *axiTimeoutFixture) awaiting() *ipc.RunInfo {
	run := fx.running()
	run.Steps = []ipc.StepResultInfo{{
		StepName: types.StepReview,
		Status:   types.StepStatusAwaitingApproval,
	}}
	return run
}

func (fx *axiTimeoutFixture) completed() *ipc.RunInfo {
	run := fx.running()
	run.Status = types.RunCompleted
	return run
}

func newAxiTimeoutFixture(t *testing.T, opts axiTimeoutOpts) *axiTimeoutFixture {
	t.Helper()
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)

	root := t.TempDir()
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "file.txt")
	cliGit(t, local, "commit", "-m", "base")
	cliGit(t, local, "checkout", "-b", "feature/timeout")
	fx := &axiTimeoutFixture{head: cliGit(t, local, "rev-parse", "HEAD")}
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		return fx.running(), nil
	})

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertRepo(registeredRoot, filepath.Join(root, "remote.git"), "main"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GateContextResult{Nested: false}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		run := fx.running()
		if opts.responded != nil && !opts.responded.Load() {
			run = fx.awaiting()
		}
		return &ipc.GetActiveRunResult{Run: run}, nil
	})
	if opts.responded != nil {
		srv.Handle(ipc.MethodRespond, func(context.Context, json.RawMessage) (interface{}, error) {
			opts.responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		})
	}
	var getRunCalls atomic.Int32
	srv.Handle(ipc.MethodGetRun, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		n := int(getRunCalls.Add(1))
		run, err := fx.callGetRun(ctx, n)
		if err != nil {
			return nil, err
		}
		return &ipc.GetRunResult{Run: run}, nil
	})
	srv.HandleStream(ipc.MethodSubscribe, hangSubscribe)
	startIPCServer(t, srv, p.Socket())
	chdir(t, local)
	return fx
}

func hangSubscribe(ctx context.Context, _ json.RawMessage) (ipc.StreamFunc, error) {
	return func(func(interface{}) error) error {
		<-ctx.Done()
		return nil
	}, nil
}

func startIPCServer(t *testing.T, srv *ipc.Server, socketPath string) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(socketPath) }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-errCh:
		case <-time.After(time.Second):
			t.Error("fake daemon did not stop")
		}
	})
	dialReady(t, socketPath).Close()
}

func dialReady(t *testing.T, socketPath string) *ipc.Client {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client, err := ipc.Dial(socketPath)
		if err == nil {
			return client
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("IPC server did not become ready")
	return nil
}

func setDriveGetRunTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := driveGetRunTimeoutNS.Swap(int64(d))
	t.Cleanup(func() { driveGetRunTimeoutNS.Store(prev) })
}

func sleepOrDone(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func assertWaitElapsed(t *testing.T, err error, out, wantWait string) {
	t.Helper()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("wait elapsed error = %v, want exit 1\n%s", err, out)
	}
	for _, want := range []string{
		"wait of " + wantWait + " elapsed",
		"not a pipeline failure",
		"no-mistakes axi status",
		"reattach",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wait-elapsed output missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"health probe failed", "database unavailable"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("idle live daemon reported as dead (%q):\n%s", forbidden, out)
		}
	}
}

var (
	builtBinOnce sync.Once
	builtBin     string
	builtBinErr  error
)

func buildNoMistakesBinary(t *testing.T) string {
	t.Helper()
	builtBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "nm-timeout-bin-")
		if err != nil {
			builtBinErr = err
			return
		}
		name := "no-mistakes"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		builtBin = filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", builtBin, "github.com/kunchenguid/no-mistakes/cmd/no-mistakes")
		cmd.Dir = repoRootFromThisFile()
		out, err := cmd.CombinedOutput()
		if err != nil {
			builtBinErr = errors.New(string(out) + ": " + err.Error())
		}
	})
	if builtBinErr != nil {
		t.Fatalf("build no-mistakes: %v", builtBinErr)
	}
	return builtBin
}

func repoRootFromThisFile() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func execNoMistakes(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := execNoMistakesErr(t, bin, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
	return out
}

func execNoMistakesErr(t *testing.T, bin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir, _ = os.Getwd()
	cmd.Env = os.Environ()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
