package gatecontext_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type topologyFixture struct {
	p       *paths.Paths
	d       *db.DB
	root    string
	work    string
	gate    string
	managed string
	origin  string
	repoID  string
}

func TestInspectorCanonicalManagedGitIdentityMatrix(t *testing.T) {
	f := newTopologyFixture(t)
	inspector := gatecontext.Inspector{DB: f.d, Paths: f.p}

	managedLink := filepath.Join(t.TempDir(), "managed-link")
	if err := os.Symlink(f.managed, managedLink); err != nil {
		t.Fatalf("symlink managed worktree: %v", err)
	}
	for _, tc := range []struct {
		name   string
		cwd    string
		marker bool
		nested bool
	}{
		{name: "managed marker present", cwd: f.managed, marker: true, nested: true},
		{name: "managed marker removed", cwd: f.managed, marker: false, nested: true},
		{name: "symlinked managed worktree", cwd: managedLink, marker: false, nested: true},
		{name: "ordinary marker forged", cwd: f.work, marker: true, nested: false},
		{name: "ordinary branch", cwd: f.work, marker: false, nested: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: tc.cwd, MarkerPresent: tc.marker})
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			if got.Nested != tc.nested {
				t.Fatalf("nested = %v, want %v (result=%+v)", got.Nested, tc.nested, got)
			}
			if got.MarkerPresent != tc.marker {
				t.Fatalf("marker evidence = %v, want %v", got.MarkerPresent, tc.marker)
			}
		})
	}

	lookalike := filepath.Join(filepath.Dir(f.p.Root()), filepath.Base(f.p.Root())+"-lookalike", "worktrees", "repo", "run")
	if err := os.MkdirAll(filepath.Dir(lookalike), 0o755); err != nil {
		t.Fatalf("mkdir lookalike parent: %v", err)
	}
	run(t, "", "git", "clone", f.origin, lookalike)
	assertAllowed(t, inspector, lookalike, "path lookalike")

	clone := filepath.Join(t.TempDir(), "independent-clone")
	run(t, "", "git", "clone", f.origin, clone)
	assertAllowed(t, inspector, clone, "independent clone")

	forkOrigin := filepath.Join(t.TempDir(), "fork.git")
	run(t, "", "git", "clone", "--bare", f.origin, forkOrigin)
	forkClone := filepath.Join(t.TempDir(), "fork-clone")
	run(t, "", "git", "clone", forkOrigin, forkClone)
	assertAllowed(t, inspector, forkClone, "fork")

	linked := filepath.Join(t.TempDir(), "linked")
	run(t, f.work, "git", "branch", "linked-branch")
	run(t, f.work, "git", "worktree", "add", linked, "linked-branch")
	assertAllowed(t, inspector, linked, "ordinary linked worktree")
}

func TestInspectorRejectsRelocatedAndSymlinkedManagedRoots(t *testing.T) {
	f := newTopologyFixture(t)
	link := filepath.Join(t.TempDir(), "nm-home-link")
	if err := os.Symlink(f.root, link); err != nil {
		t.Fatalf("symlink root: %v", err)
	}
	inspector := gatecontext.Inspector{DB: f.d, Paths: paths.WithRoot(link)}
	got, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: f.managed})
	if err != nil {
		t.Fatalf("inspect symlinked root: %v", err)
	}
	if !got.Nested || !got.ManagedGit {
		t.Fatalf("symlinked relocated root not rejected: %+v", got)
	}
}

func TestInspectorPreMigrationSchemaDegradesActiveAgentSteps(t *testing.T) {
	cases := []struct {
		name      string
		dropRuns  []string
		dropSteps []string
		withRun   bool
		wantErr   string
	}{
		{
			name:     "runs missing review_approved_head_sha degrades to empty",
			dropRuns: []string{"review_approved_head_sha"},
		},
		{
			name:     "runs missing submitted_head_sha degrades to empty",
			dropRuns: []string{"submitted_head_sha"},
		},
		{
			name:     "runs missing awaiting_agent_since degrades to empty",
			dropRuns: []string{"awaiting_agent_since"},
		},
		{
			name:     "runs missing parked_ms degrades to empty",
			dropRuns: []string{"parked_ms"},
		},
		{
			name:     "runs missing any other migration column degrades to empty",
			dropRuns: []string{"intent"},
		},
		{
			name:      "step_results missing migration columns degrades to empty",
			dropSteps: []string{"last_activity_at", "last_activity", "agent_pid", "auto_fix_limit"},
			withRun:   true,
		},
		{
			name:      "step_results missing single migration column degrades to empty",
			dropSteps: []string{"auto_fix_limit"},
			withRun:   true,
		},
		{
			name:     "runs missing unrelated column propagates",
			dropRuns: []string{"status"},
			wantErr:  "no such column: status",
		},
		{
			name:      "step_results missing unrelated column propagates",
			dropSteps: []string{"status"},
			withRun:   true,
			wantErr:   "no such column: status",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "old.sqlite")
			database, err := db.Open(dbPath)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			if err := database.Close(); err != nil {
				t.Fatalf("close db: %v", err)
			}
			raw, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatalf("open raw db: %v", err)
			}
			for _, col := range tc.dropRuns {
				if _, err := raw.Exec(`ALTER TABLE runs DROP COLUMN ` + col); err != nil {
					raw.Close()
					t.Fatalf("drop runs column %s: %v", col, err)
				}
			}
			for _, col := range tc.dropSteps {
				if _, err := raw.Exec(`ALTER TABLE step_results DROP COLUMN ` + col); err != nil {
					raw.Close()
					t.Fatalf("drop step_results column %s: %v", col, err)
				}
			}
			if tc.withRun {
				if _, err := raw.Exec(`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at) VALUES ('run-1', 'repo-1', 'feature', 'head', 'base', 'pending', 1, 1)`); err != nil {
					raw.Close()
					t.Fatalf("insert run: %v", err)
				}
			}
			if err := raw.Close(); err != nil {
				t.Fatalf("close raw db: %v", err)
			}

			readonly, err := db.OpenReadOnly(dbPath)
			if err != nil {
				t.Fatalf("open read-only: %v", err)
			}
			defer readonly.Close()
			inspector := gatecontext.Inspector{DB: readonly, Paths: paths.WithRoot(t.TempDir())}
			got, err := inspector.Inspect(context.Background(), gatecontext.Request{})
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("inspect = %+v, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("inspect error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("inspect on pre-migration schema: %v", err)
			}
			if got.Nested {
				t.Fatalf("pre-migration schema classified as nested: %+v", got)
			}
		})
	}
}

func TestInspectorUsesAuthenticatedProcessAncestryAfterCWDChange(t *testing.T) {
	f := newTopologyFixture(t)
	runRecord, err := f.d.InsertRun(f.repoID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := f.d.UpdateRunStatus(runRecord.ID, types.RunRunning); err != nil {
		t.Fatalf("start run: %v", err)
	}
	step, err := f.d.InsertStepResult(runRecord.ID, types.StepDocument)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if err := f.d.StartStep(step.ID); err != nil {
		t.Fatalf("start step: %v", err)
	}
	agentPID := 4100
	if err := f.d.SetStepAgentActivity(step.ID, "started", &agentPID); err != nil {
		t.Fatalf("set agent pid: %v", err)
	}
	parents := map[int]int{4300: 4200, 4200: agentPID, agentPID: 1}
	inspector := gatecontext.Inspector{
		DB:    f.d,
		Paths: f.p,
		ParentPID: func(pid int) (int, error) {
			return parents[pid], nil
		},
	}
	got, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: f.work, PeerPID: 4300})
	if err != nil {
		t.Fatalf("inspect descendant: %v", err)
	}
	if !got.Nested || !got.AgentDescendant || got.RunID != runRecord.ID || got.Phase != types.StepDocument {
		t.Fatalf("descendant classification = %+v", got)
	}

	ordinary, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: f.work, PeerPID: 9000, MarkerPresent: true})
	if err != nil {
		t.Fatalf("inspect ordinary: %v", err)
	}
	if ordinary.Nested {
		t.Fatalf("independent ordinary peer rejected by forged marker: %+v", ordinary)
	}

	inspector.ParentPID = func(pid int) (int, error) {
		if pid == 9300 {
			return 5000, nil
		}
		return 1, nil
	}
	daemonChild, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: f.work, PeerPID: 9300, DaemonPID: 5000})
	if err != nil {
		t.Fatalf("inspect daemon child: %v", err)
	}
	if !daemonChild.Nested || !daemonChild.DaemonDescendant || daemonChild.RunID != "" {
		t.Fatalf("daemon-descendant classification = %+v, want refusal without guessed run metadata", daemonChild)
	}
}

func TestInspectorConcurrentClassificationIsDeterministic(t *testing.T) {
	f := newTopologyFixture(t)
	inspector := gatecontext.Inspector{DB: f.d, Paths: f.p}
	const workers = 24
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: f.managed})
			if err != nil {
				errs <- err
				return
			}
			if !got.Nested || !got.ManagedGit {
				errs <- &classificationError{got: got}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

type classificationError struct{ got gatecontext.Result }

func (e *classificationError) Error() string { return "non-deterministic classification" }

func assertAllowed(t *testing.T, inspector gatecontext.Inspector, cwd, label string) {
	t.Helper()
	got, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: cwd})
	if err != nil {
		t.Fatalf("inspect %s: %v", label, err)
	}
	if got.Nested {
		t.Fatalf("%s rejected: %+v", label, got)
	}
}

func newTopologyFixture(t *testing.T) *topologyFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "relocated-nm-home")
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure paths: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	origin := filepath.Join(t.TempDir(), "origin.git")
	run(t, "", "git", "init", "--bare", "--initial-branch=main", origin)
	work := filepath.Join(t.TempDir(), "work")
	initOrdinaryRepo(t, work, origin)
	repo, _, err := gate.Init(context.Background(), database, p, work)
	if err != nil {
		t.Fatalf("init gate: %v", err)
	}
	gateDir := p.RepoDir(repo.ID)
	run(t, gateDir, "git", "fetch", work, "HEAD:refs/heads/feature")
	managed := filepath.Join(p.WorktreesDir(), repo.ID, "managed-run")
	run(t, gateDir, "git", "worktree", "add", "--detach", managed, "refs/heads/feature")
	return &topologyFixture{p: p, d: database, root: root, work: work, gate: gateDir, managed: managed, origin: origin, repoID: repo.ID}
}

func initOrdinaryRepo(t *testing.T, dir, origin string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	run(t, dir, "git", "init", "--initial-branch=main")
	run(t, dir, "git", "config", "user.email", "test@example.com")
	run(t, dir, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	run(t, dir, "git", "add", "README.md")
	run(t, dir, "git", "commit", "-m", "base")
	run(t, dir, "git", "remote", "add", "origin", origin)
	run(t, dir, "git", "push", "-u", "origin", "main")
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_CONFIG_COUNT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", name, args, dir, err, out)
	}
}
