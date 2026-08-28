package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/wizard"
)

// TestFreshRunBranchOwnershipDistinguishesMissingTerminalHead reproduces the
// custody deadlock caused by a terminal run whose moved head is no longer
// available. That state must remain manual-only for recovery, while it must
// not keep unrelated fresh work behind a custody claim for commits that no
// longer exist.
func TestFreshRunBranchOwnershipDistinguishesMissingTerminalHead(t *testing.T) {
	for _, tc := range []struct {
		name             string
		recordedHead     func(t *testing.T, submitted string) string
		advanceWorktree  bool
		gatePresent      bool
		olderRecoverable bool
		survivingAnchor  bool
		verifiedHead     bool
		objectReadError  bool
		wantFreshBlocked bool
		wantSafety       string
	}{
		{
			name: "terminal unmoved releases branch",
			recordedHead: func(_ *testing.T, submitted string) string {
				return submitted
			},
			verifiedHead:     true,
			wantFreshBlocked: false,
			wantSafety:       "user_owned",
		},
		{
			name: "terminal unmoved without verification keeps custody",
			recordedHead: func(_ *testing.T, submitted string) string {
				return submitted
			},
			verifiedHead:     false,
			wantFreshBlocked: true,
			wantSafety:       "blocked_pipeline_owned_recoverable",
		},
		{
			name: "terminal recoverable moved head keeps custody",
			recordedHead: func(_ *testing.T, submitted string) string {
				return submitted
			},
			advanceWorktree:  true,
			verifiedHead:     true,
			wantFreshBlocked: true,
			wantSafety:       "blocked_pipeline_owned_recoverable",
		},
		{
			name: "terminal missing moved head releases fresh path",
			recordedHead: func(_ *testing.T, _ string) string {
				return strings.Repeat("f", 40)
			},
			gatePresent:      true,
			verifiedHead:     true,
			wantFreshBlocked: false,
			wantSafety:       "blocked_recover_preserved_head_missing",
		},
		{
			name: "missing gate keeps fresh path blocked",
			recordedHead: func(_ *testing.T, _ string) string {
				return strings.Repeat("f", 40)
			},
			wantFreshBlocked: true,
			wantSafety:       "blocked_recover_preserved_head_missing",
		},
		{
			name: "unverified missing head keeps fresh path blocked",
			recordedHead: func(_ *testing.T, _ string) string {
				return strings.Repeat("f", 40)
			},
			gatePresent:      true,
			verifiedHead:     false,
			wantFreshBlocked: true,
			wantSafety:       "blocked_recover_preserved_head_missing",
		},
		{
			name: "missing head with recovery evidence keeps custody",
			recordedHead: func(_ *testing.T, _ string) string {
				return strings.Repeat("f", 40)
			},
			gatePresent:      true,
			survivingAnchor:  true,
			wantFreshBlocked: true,
			wantSafety:       "blocked_recover_preserved_head_missing",
		},
		{
			name: "older recoverable head keeps custody",
			recordedHead: func(_ *testing.T, _ string) string {
				return strings.Repeat("f", 40)
			},
			gatePresent:      true,
			olderRecoverable: true,
			wantFreshBlocked: true,
			wantSafety:       "blocked_recover_preserved_head_missing",
		},
		{
			name: "object read failure keeps custody",
			recordedHead: func(_ *testing.T, _ string) string {
				return strings.Repeat("f", 40)
			},
			gatePresent:      true,
			objectReadError:  true,
			wantFreshBlocked: true,
			wantSafety:       "blocked_recover_preserved_head_missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir, paths, database, repo := setupAxiQueryRepo(t)
			cliGit(t, repoDir, "checkout", "-b", "feature/missing-head")
			chdir(t, repoDir)

			submitted := cliGit(t, repoDir, "rev-parse", "HEAD")
			recorded := tc.recordedHead(t, submitted)
			var gateDir string
			if tc.gatePresent {
				gateDir = paths.RepoDir(repo.ID)
				if err := os.MkdirAll(gateDir, 0o755); err != nil {
					t.Fatalf("create gate directory: %v", err)
				}
				cliGit(t, gateDir, "init", "--bare")
			}
			if tc.olderRecoverable {
				cliGit(t, repoDir, "commit", "--allow-empty", "-m", "older pipeline fix")
				olderHead := cliGit(t, repoDir, "rev-parse", "HEAD")
				cliGit(t, repoDir, "push", gateDir, "HEAD:refs/heads/feature/missing-head")
				olderRun, err := database.InsertRun(repo.ID, "feature/missing-head", submitted, submitted)
				if err != nil {
					t.Fatalf("insert older pipeline run: %v", err)
				}
				if err := database.UpdateRunStatusWithVerifiedHead(olderRun.ID, types.RunCancelled, olderHead); err != nil {
					t.Fatalf("terminalize older pipeline run: %v", err)
				}
			}
			if tc.advanceWorktree {
				cliGit(t, repoDir, "commit", "--allow-empty", "-m", "pipeline fix")
				recorded = cliGit(t, repoDir, "rev-parse", "HEAD")
			}
			if tc.gatePresent && !tc.olderRecoverable {
				cliGit(t, repoDir, "push", gateDir, "HEAD:refs/heads/feature/missing-head")
				if tc.objectReadError {
					objectPath := filepath.Join(gateDir, "objects", recorded[:2], recorded[2:])
					if err := os.MkdirAll(filepath.Dir(objectPath), 0o755); err != nil {
						t.Fatalf("create corrupt object directory: %v", err)
					}
					if err := os.WriteFile(objectPath, []byte("corrupt object"), 0o644); err != nil {
						t.Fatalf("write corrupt object: %v", err)
					}
				}
			}

			pipelineRun, err := database.InsertRun(repo.ID, "feature/missing-head", submitted, submitted)
			if err != nil {
				t.Fatalf("insert pipeline run: %v", err)
			}
			if err := database.UpdateRunHeadSHA(pipelineRun.ID, recorded); err != nil {
				t.Fatalf("record pipeline head: %v", err)
			}
			if tc.verifiedHead {
				if err := database.UpdateRunStatusWithVerifiedHead(pipelineRun.ID, types.RunCancelled, recorded); err != nil {
					t.Fatalf("terminalize pipeline run: %v", err)
				}
			} else if err := database.UpdateRunStatus(pipelineRun.ID, types.RunCancelled); err != nil {
				t.Fatalf("terminalize pipeline run: %v", err)
			}
			if tc.survivingAnchor {
				cliGit(t, paths.RepoDir(repo.ID), "update-ref", custody.RecoveryRef(pipelineRun.ID), submitted)
			}

			env := &axiEnv{p: paths, d: database, repo: repo, cfg: config.DefaultGlobalConfig()}
			state := inspectAxiBranchSync(context.Background(), env)
			if state.Safety != tc.wantSafety {
				t.Fatalf("branch ownership state = %s, want %s: %#v", state.Safety, tc.wantSafety, state)
			}
			blocked := freshRunBranchOwnershipState(context.Background(), env)
			if (blocked != nil) != tc.wantFreshBlocked {
				t.Fatalf("fresh-run ownership = %#v, blocked = %t, want blocked = %t", blocked, blocked != nil, tc.wantFreshBlocked)
			}
			if tc.wantFreshBlocked && blocked.NextAction == nil {
				t.Fatal("recoverable terminal head lost its custody guidance")
			}
			if tc.wantSafety == "blocked_recover_preserved_head_missing" &&
				(state.NextAction == nil || state.NextAction.Code != "inspect_and_reconcile_manually") {
				t.Fatalf("missing-head state lost manual reconciliation guidance: %#v", state)
			}
		})
	}
}

type missingHeadFreshRunFixture struct {
	repoDir string
	paths   *paths.Paths
	d       *db.DB
	repo    *db.Repo
	branch  string
	head    string
}

func newMissingHeadFreshRunFixture(t *testing.T) missingHeadFreshRunFixture {
	t.Helper()
	repoDir := setupTestRepo(t)
	p := paths.WithRoot(os.Getenv("NM_HOME"))
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	repo, _, err := gate.Init(context.Background(), d, p, ".")
	if err != nil {
		t.Fatal(err)
	}

	branch := "feature/missing-head"
	run(t, repoDir, "git", "checkout", "-b", branch)
	head := cliGit(t, repoDir, "rev-parse", "HEAD")
	cliGit(t, p.RepoDir(repo.ID), "fetch", repoDir, "HEAD:refs/heads/"+branch)
	missing := strings.Repeat("f", 40)
	pipelineRun, err := d.InsertRun(repo.ID, branch, head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(pipelineRun.ID, types.RunCancelled, missing); err != nil {
		t.Fatal(err)
	}

	mockClaude := writeMockClaude(t, t.TempDir())
	configYAML := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	startTestDaemon(t, p, d)

	return missingHeadFreshRunFixture{repoDir: repoDir, paths: p, d: d, repo: repo, branch: branch, head: head}
}

func TestTriggerRunStartsFreshRunWhenNoopGateHasMissingTerminalHead(t *testing.T) {
	f := newMissingHeadFreshRunFixture(t)
	client, err := ipc.Dial(f.paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	env := &axiEnv{p: f.paths, d: f.d, repo: f.repo, cfg: config.DefaultGlobalConfig(), client: client}
	runID, err := triggerRun(context.Background(), env, f.branch, f.head, nil, "fresh delivery")
	if err != nil {
		t.Fatalf("triggerRun() error = %v", err)
	}
	got, err := f.d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.HeadSHA != f.head {
		t.Fatalf("fresh run = %#v, want head %s", got, f.head)
	}
}

func TestRootWizardStartsFreshRunWhenNoopGateHasMissingTerminalHead(t *testing.T) {
	f := newMissingHeadFreshRunFixture(t)

	previousInteractive := terminalInteractive
	terminalInteractive = func() bool { return false }
	defer func() { terminalInteractive = previousInteractive }()
	previousTimeout := triggerWaitTimeout
	triggerWaitTimeout = 100 * time.Millisecond
	defer func() { triggerWaitTimeout = previousTimeout }()

	previousRunTUI := runTUI
	var attached *ipc.RunInfo
	runTUI = func(_ string, _ *ipc.Client, run *ipc.RunInfo, _ string) error {
		attached = run
		return nil
	}
	defer func() { runTUI = previousRunTUI }()

	if _, err := executeCmd("-y"); err != nil {
		t.Fatalf("executeCmd(-y) error = %v", err)
	}
	if attached == nil || attached.HeadSHA != f.head {
		t.Fatalf("wizard attached run = %#v, want head %s", attached, f.head)
	}
}

func TestRootWizardKeepsRunIdentityAfterTerminalWait(t *testing.T) {
	f := newMissingHeadFreshRunFixture(t)

	previousInteractive := terminalInteractive
	terminalInteractive = func() bool { return true }
	defer func() { terminalInteractive = previousInteractive }()
	previousTimeout := triggerWaitTimeout
	triggerWaitTimeout = 100 * time.Millisecond
	defer func() { triggerWaitTimeout = previousTimeout }()

	previousWizardRun := wizardRun
	var startedRunID string
	wizardRun = func(cfg wizard.Config) (wizard.Result, error) {
		if err := cfg.Push(context.Background(), f.branch); err != nil {
			return wizard.Result{}, err
		}
		if err := cfg.WaitForRun(context.Background(), f.branch); err != nil {
			return wizard.Result{}, err
		}
		runs, err := f.d.GetRunsByRepo(f.repo.ID)
		if err != nil {
			return wizard.Result{}, err
		}
		startedRunID = runs[0].ID
		if err := f.d.UpdateRunStatus(startedRunID, types.RunCompleted); err != nil {
			return wizard.Result{}, err
		}
		return wizard.Result{Success: true, Pushed: true, TargetBranch: f.branch}, nil
	}
	defer func() { wizardRun = previousWizardRun }()

	previousRunTUI := runTUI
	var attached *ipc.RunInfo
	runTUI = func(_ string, _ *ipc.Client, run *ipc.RunInfo, _ string) error {
		attached = run
		return nil
	}
	defer func() { runTUI = previousRunTUI }()

	if _, err := executeCmd(); err != nil {
		t.Fatalf("executeCmd() error = %v", err)
	}
	if attached == nil || attached.ID != startedRunID {
		t.Fatalf("wizard attached run = %#v, want run %s", attached, startedRunID)
	}
	runs, err := f.d.GetRunsByRepo(f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs after terminal wizard handoff = %d, want original plus one fresh run", len(runs))
	}
}

func TestDaemonFreshRunRechecksPipelineCustody(t *testing.T) {
	f := newMissingHeadFreshRunFixture(t)
	cliGit(t, f.repoDir, "commit", "--allow-empty", "-m", "recoverable pipeline fix")
	movedHead := cliGit(t, f.repoDir, "rev-parse", "HEAD")
	cliGit(t, f.paths.RepoDir(f.repo.ID), "fetch", f.repoDir, "HEAD:refs/heads/"+f.branch)
	recoverable, err := f.d.InsertRun(f.repo.ID, f.branch, f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatusWithVerifiedHead(recoverable.ID, types.RunCancelled, movedHead); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(f.paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.StartFreshRunResult
	err = client.Call(ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{
		RepoID:  f.repo.ID,
		Branch:  f.branch,
		HeadSHA: movedHead,
		WorkDir: f.repoDir,
	}, &result)
	if err == nil || !strings.Contains(err.Error(), "fresh run blocked") {
		t.Fatalf("fresh run IPC error = %v, want custody refusal", err)
	}
	runs, err := f.d.GetRunsByRepo(f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs after custody refusal = %d, want 2", len(runs))
	}
}

func TestDaemonFreshRunReturnsNewExactHeadRunInsteadOfDuplicating(t *testing.T) {
	f := newMissingHeadFreshRunFixture(t)
	runs, err := f.d.GetRunsByRepo(f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	priorRunIDs := make([]string, 0, len(runs))
	for _, run := range runs {
		priorRunIDs = append(priorRunIDs, run.ID)
	}
	existing, err := f.d.InsertRun(f.repo.ID, f.branch, f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatusWithVerifiedHead(existing.ID, types.RunCompleted, f.head); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(f.paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.StartFreshRunResult
	err = client.Call(ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{
		RepoID:      f.repo.ID,
		Branch:      f.branch,
		HeadSHA:     f.head,
		WorkDir:     f.repoDir,
		PriorRunIDs: priorRunIDs,
	}, &result)
	if err != nil {
		t.Fatalf("fresh run IPC error = %v", err)
	}
	if result.RunID != existing.ID {
		t.Fatalf("fresh run ID = %s, want existing exact-head run %s", result.RunID, existing.ID)
	}
	runs, err = f.d.GetRunsByRepo(f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != len(priorRunIDs)+1 {
		t.Fatalf("runs after exact-head handoff = %d, want %d", len(runs), len(priorRunIDs)+1)
	}
}

func TestWizardFreshRunRefusesContextDriftAfterPush(t *testing.T) {
	f := newMissingHeadFreshRunFixture(t)
	runs, err := f.d.GetRunsByRepo(f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	priorRunIDs := make(map[string]struct{}, len(runs))
	for _, run := range runs {
		priorRunIDs[run.ID] = struct{}{}
	}
	identity := &freshRunIdentity{branch: f.branch, headSHA: f.head, priorRunIDs: priorRunIDs}

	cliGit(t, f.repoDir, "checkout", "-b", "feature/context-drift")
	previousTimeout := triggerWaitTimeout
	triggerWaitTimeout = 100 * time.Millisecond
	defer func() { triggerWaitTimeout = previousTimeout }()
	client, err := ipc.Dial(f.paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := awaitDaemonRunRegistrationOrStartFresh(context.Background(), client, f.paths, f.d, f.repo, f.repoDir, f.branch, identity, nil); err == nil || !strings.Contains(err.Error(), "context changed") {
		t.Fatalf("context-drift handoff error = %v, want context refusal", err)
	}
	runs, err = f.d.GetRunsByRepo(f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != len(priorRunIDs) {
		t.Fatalf("runs after context-drift refusal = %d, want %d", len(runs), len(priorRunIDs))
	}
}
