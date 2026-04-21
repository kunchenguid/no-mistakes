package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

var applyShellEnvToProcess = shellenv.ApplyToProcess
var createDaemonPIDTempFile = os.CreateTemp
var renameDaemonPIDFile = os.Rename

// Run starts the daemon process. It blocks until a shutdown signal is received
// or the shutdown IPC method is called. This is called when NM_DAEMON=1 or via
// the hidden `no-mistakes daemon run` entrypoint used by the managed service.
func Run() error {
	p, err := paths.New()
	if err != nil {
		return fmt.Errorf("resolve paths: %w", err)
	}
	if err := p.EnsureDirs(); err != nil {
		return fmt.Errorf("create directories: %w", err)
	}
	if err := prepareDaemonEnvironment(); err != nil {
		return err
	}

	// Ensure default config exists, then load it.
	config.EnsureDefaultGlobalConfig(p.ConfigFile())
	globalCfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	initLogger(globalCfg.LogLevel)

	d, err := db.Open(p.DB())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer d.Close()

	return RunWithResources(p, d)
}

func prepareDaemonEnvironment() error {
	nmHome := os.Getenv("NM_HOME")
	for _, key := range []string{
		"CLAUDECODE",
		"CLAUDE_CODE_ENTRYPOINT",
		"CLAUDE_CODE_ENTRY_POINT",
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_SESSION_ACCESS_TOKEN",
	} {
		if err := os.Unsetenv(key); err != nil {
			return fmt.Errorf("unset %s: %w", key, err)
		}
	}
	if err := applyShellEnvToProcess(); err != nil {
		return fmt.Errorf("apply login shell environment: %w", err)
	}
	if nmHome != "" {
		if err := os.Setenv("NM_HOME", nmHome); err != nil {
			return fmt.Errorf("restore NM_HOME: %w", err)
		}
	}
	return nil
}

// initLogger sets up the global slog handler with the configured log level.
func initLogger(level string) {
	lvl := config.ParseLogLevel(level)
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler))
}

// RunWithResources starts the daemon with pre-initialized paths and DB.
// Useful for testing where the caller controls resource setup.
func RunWithResources(p *paths.Paths, d *db.DB) error {
	return RunWithOptions(p, d, nil)
}

// RunWithOptions starts the daemon with optional overrides.
// stepFactory overrides the default pipeline steps (for testing).
func RunWithOptions(p *paths.Paths, d *db.DB, stepFactory StepFactory) error {
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
		defer cancel()
		_ = telemetry.Close(ctx)
	}()

	// Recover stale runs from a previous daemon crash.
	recoverOnStartup(d, p)

	// Point the agent package at our PID tracking dir so any managed
	// servers we spawn from here on leave crash-recovery breadcrumbs.
	agent.SetServerPIDsDir(p.ServerPIDsDir())
	defer agent.SetServerPIDsDir("")

	srv := ipc.NewServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := NewRunManager(d, p, stepFactory)

	var shutdownOnce sync.Once
	doShutdown := func(reason string) {
		shutdownOnce.Do(func() {
			slog.Info("shutting down", "reason", reason)
			mgr.Shutdown()
			cancel()
			srv.Close()
		})
	}

	registerHandlers(srv, mgr, d, func() { doShutdown("ipc request") })

	// Write PID file
	pidPath := p.PIDFile()
	pidRecord, err := currentDaemonPIDRecord(processStartTime, func() time.Time { return time.Now().UTC() })
	if err != nil {
		return fmt.Errorf("build pid file: %w", err)
	}
	if err := writeDaemonPIDFile(pidPath, pidRecord); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	defer func() {
		if pidData, err := os.ReadFile(pidPath); err == nil {
			if current, readErr := readDaemonPIDFileData(pidData); readErr == nil && current.PID == pidRecord.PID && current.StartedAt.Equal(pidRecord.StartedAt) {
				os.Remove(pidPath)
			}
		}
	}()

	// Handle OS signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, daemonSignals()...)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case sig := <-sigCh:
			doShutdown(sig.String())
		case <-ctx.Done():
		}
	}()

	socketPath := p.Socket()
	slog.Info("daemon starting", "socket", socketPath, "pid", os.Getpid())

	if err := srv.Serve(socketPath); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	doShutdown("listener closed")

	// Clean up socket file only if we still own the PID file.
	// A new daemon may have already replaced the socket.
	if pidData, err := os.ReadFile(pidPath); err == nil {
		if current, readErr := readDaemonPIDFileData(pidData); readErr == nil && current.PID == pidRecord.PID && current.StartedAt.Equal(pidRecord.StartedAt) {
			os.Remove(pidPath)
			os.Remove(socketPath)
		}
	}
	slog.Info("daemon stopped")
	return nil
}

func currentDaemonPIDRecord(startTime func(int) (time.Time, error), now func() time.Time) (daemonPIDFile, error) {
	pid := os.Getpid()
	startedAt, err := startTime(pid)
	if err != nil {
		startedAt = agent.CurrentProcessStartedAt()
		if startedAt.IsZero() {
			startedAt = now()
		}
	}
	return daemonPIDFile{PID: pid, StartedAt: startedAt.UTC()}, nil
}

func writeDaemonPIDFile(path string, record daemonPIDFile) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal pid file: %w", err)
	}
	tmp, err := createDaemonPIDTempFile(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create pid temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod pid temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write pid temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close pid temp file: %w", err)
	}
	if err := renameDaemonPIDFile(tmpPath, path); err != nil {
		return fmt.Errorf("rename pid file: %w", err)
	}
	tmpPath = ""
	return nil
}

// recoverOnStartup cleans up after a previous daemon crash by marking stale
// runs/steps as failed, killing orphaned managed-server subprocesses
// (opencode, rovodev), and removing orphaned worktree directories.
func recoverOnStartup(d *db.DB, p *paths.Paths) {
	reapOrphanedServers(p)

	count, err := d.RecoverStaleRuns("daemon crashed during execution")
	if err != nil {
		slog.Error("failed to recover stale runs", "error", err)
		return
	}
	if count > 0 {
		slog.Info("recovered stale runs from previous crash", "count", count)
	}

	// Clean up orphaned worktree directories.
	wtRoot := p.WorktreesDir()
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		return // directory may not exist yet
	}
	ctx := context.Background()
	for _, repoEntry := range entries {
		if !repoEntry.IsDir() {
			continue
		}
		repoPath := filepath.Join(wtRoot, repoEntry.Name())
		gateDir := p.RepoDir(repoEntry.Name())
		runEntries, err := os.ReadDir(repoPath)
		if err != nil {
			continue
		}
		for _, runEntry := range runEntries {
			if !runEntry.IsDir() {
				continue
			}
			wtPath := filepath.Join(repoPath, runEntry.Name())
			if err := git.WorktreeRemove(ctx, gateDir, wtPath); err != nil {
				slog.Warn("git worktree remove failed, falling back to os.RemoveAll", "path", wtPath, "error", err)
				if err := os.RemoveAll(wtPath); err != nil {
					slog.Warn("failed to remove orphaned worktree", "path", wtPath, "error", err)
				}
			} else {
				slog.Info("removed orphaned worktree", "path", wtPath)
			}
		}
		// Remove empty repo dir.
		os.Remove(repoPath)
	}
}

func registerHandlers(srv *ipc.Server, mgr *RunManager, d *db.DB, shutdown func()) {
	srv.Handle(ipc.MethodHealth, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})

	srv.Handle(ipc.MethodShutdown, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		go shutdown()
		return &ipc.ShutdownResult{OK: true}, nil
	})

	srv.Handle(ipc.MethodGetRun, func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.GetRunParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		run, err := d.GetRun(p.RunID)
		if err != nil {
			return nil, fmt.Errorf("get run: %w", err)
		}
		if run == nil {
			return nil, fmt.Errorf("run not found: %s", p.RunID)
		}
		steps, err := d.GetStepsByRun(p.RunID)
		if err != nil {
			return nil, fmt.Errorf("get steps: %w", err)
		}
		return &ipc.GetRunResult{Run: runToInfo(run, steps)}, nil
	})

	srv.Handle(ipc.MethodGetRuns, func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.GetRunsParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		runs, err := d.GetRunsByRepo(p.RepoID)
		if err != nil {
			return nil, fmt.Errorf("get runs: %w", err)
		}
		infos := make([]ipc.RunInfo, 0, len(runs))
		for _, r := range runs {
			steps, err := d.GetStepsByRun(r.ID)
			if err != nil {
				return nil, fmt.Errorf("get steps for run %s: %w", r.ID, err)
			}
			infos = append(infos, *runToInfo(r, steps))
		}
		return &ipc.GetRunsResult{Runs: infos}, nil
	})

	srv.Handle(ipc.MethodGetActiveRun, func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.GetActiveRunParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		run, err := d.GetActiveRun(p.RepoID, p.Branch)
		if err != nil {
			return nil, fmt.Errorf("get active run: %w", err)
		}
		if run == nil {
			return &ipc.GetActiveRunResult{}, nil
		}
		steps, err := d.GetStepsByRun(run.ID)
		if err != nil {
			return nil, fmt.Errorf("get steps: %w", err)
		}
		return &ipc.GetActiveRunResult{Run: runToInfo(run, steps)}, nil
	})

	srv.Handle(ipc.MethodRerun, func(ctx context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.RerunParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		runID, err := mgr.HandleRerun(ctx, p.RepoID, p.Branch)
		if err != nil {
			return nil, err
		}
		return &ipc.RerunResult{RunID: runID}, nil
	})

	srv.Handle(ipc.MethodPushReceived, func(ctx context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.PushReceivedParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		slog.Info("push received", "ref", p.Ref, "old", p.Old, "new", p.New, "gate", p.Gate)
		runID, err := mgr.HandlePushReceived(ctx, &p)
		if err != nil {
			return nil, err
		}
		return &ipc.PushReceivedResult{RunID: runID}, nil
	})

	srv.Handle(ipc.MethodRespond, func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.RespondParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if err := mgr.HandleRespond(p.RunID, p.Step, p.Action, p.FindingIDs); err != nil {
			return nil, err
		}
		return &ipc.RespondResult{OK: true}, nil
	})

	srv.Handle(ipc.MethodCancelRun, func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.CancelRunParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if err := mgr.HandleCancel(p.RunID); err != nil {
			return nil, err
		}
		return &ipc.CancelRunResult{OK: true}, nil
	})

	srv.HandleStream(ipc.MethodSubscribe, func(ctx context.Context, params json.RawMessage, send func(interface{}) error) error {
		var p ipc.SubscribeParams
		if err := json.Unmarshal(params, &p); err != nil {
			return fmt.Errorf("invalid params: %w", err)
		}

		ch, unsub := mgr.Subscribe(p.RunID)
		defer unsub()

		for {
			select {
			case event, ok := <-ch:
				if !ok {
					return nil // channel closed (run completed)
				}
				if err := send(event); err != nil {
					return err // client disconnected
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
}

func runToInfo(r *db.Run, steps []*db.StepResult) *ipc.RunInfo {
	info := &ipc.RunInfo{
		ID:        r.ID,
		RepoID:    r.RepoID,
		Branch:    r.Branch,
		HeadSHA:   r.HeadSHA,
		BaseSHA:   r.BaseSHA,
		Status:    r.Status,
		PRURL:     r.PRURL,
		Error:     r.Error,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
	if len(steps) > 0 {
		info.Steps = make([]ipc.StepResultInfo, 0, len(steps))
		for _, s := range steps {
			info.Steps = append(info.Steps, stepToInfo(s))
		}
	}
	return info
}

func stepToInfo(s *db.StepResult) ipc.StepResultInfo {
	return ipc.StepResultInfo{
		ID:           s.ID,
		RunID:        s.RunID,
		StepName:     s.StepName,
		StepOrder:    s.StepOrder,
		Status:       s.Status,
		ExitCode:     s.ExitCode,
		DurationMS:   s.DurationMS,
		FindingsJSON: s.FindingsJSON,
		Error:        s.Error,
		StartedAt:    s.StartedAt,
		CompletedAt:  s.CompletedAt,
	}
}
