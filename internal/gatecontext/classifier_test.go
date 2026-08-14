package gatecontext_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
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

// TestInspectorAttributesRunInConfiguredWorktreeRoot covers a repository whose
// run worktrees the operator placed in a directory of their own
// (worktree_roots): the path no longer spells out <NM_HOME>/worktrees, so
// run/phase attribution comes from the placement the run recorded. Without it
// the caller is still refused as managed Git, but the refusal cannot name what
// it is standing in - and reading the record rather than the configuration is
// what keeps that naming working when the global config cannot be read at all.
func TestInspectorAttributesRunInConfiguredWorktreeRoot(t *testing.T) {
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

	root := filepath.Join(t.TempDir(), "repo-runs")
	managed := filepath.Join(root, runRecord.ID)
	run(t, f.gate, "git", "worktree", "add", "--detach", managed, "refs/heads/feature")
	if err := f.d.SetRunWorktreeDir(runRecord.ID, managed); err != nil {
		t.Fatalf("record placement: %v", err)
	}
	// An unreadable global config must not cost the refusal its run metadata.
	if err := os.WriteFile(f.p.ConfigFile(), []byte("worktree_roots: [not, a, mapping\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	inspector := gatecontext.Inspector{DB: f.d, Paths: f.p}
	got, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: managed})
	if err != nil {
		t.Fatalf("inspect configured worktree: %v", err)
	}
	if !got.Nested || !got.ManagedGit {
		t.Fatalf("worktree in a configured root not rejected: %+v", got)
	}
	if got.RunID != runRecord.ID || got.Phase != types.StepDocument {
		t.Fatalf("run attribution = (%q, %q), want (%q, %q)", got.RunID, got.Phase, runRecord.ID, types.StepDocument)
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

// TestInspectorClassifiesAgainstADatabaseOlderThanTheBinary is the upgrade path.
// This classifier runs in the CLI preflight of every pipeline-control command,
// before anything opens the database read-write - so on the first invocation
// after an upgrade it reads a schema this binary has not migrated, through a
// read-only handle that deliberately will not migrate it. A query naming a column
// that schema lacks would fail here, and since the commands it guards include the
// ones that perform the migration, the whole CLI would be stuck until the file was
// repaired by hand.
func TestInspectorClassifiesAgainstADatabaseOlderThanTheBinary(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// The runs table as an older release wrote it: no worktree_dir, and an
	// active run in it.
	if _, err := legacy.Exec(`
		CREATE TABLE repos (id TEXT PRIMARY KEY, working_path TEXT NOT NULL UNIQUE, upstream_url TEXT NOT NULL, default_branch TEXT NOT NULL DEFAULT 'main', created_at INTEGER NOT NULL);
		CREATE TABLE runs (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, branch TEXT NOT NULL, head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', pr_url TEXT, error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		CREATE TABLE step_results (id TEXT PRIMARY KEY, run_id TEXT NOT NULL, step_name TEXT NOT NULL, step_order INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'pending', exit_code INTEGER, duration_ms INTEGER, log_path TEXT, findings_json TEXT, error TEXT, started_at INTEGER, completed_at INTEGER, last_activity_at INTEGER, last_activity TEXT, agent_pid INTEGER, auto_fix_limit INTEGER);
		INSERT INTO repos VALUES ('repo-1', '/work/repo', 'https://example.com/repo.git', 'main', 1);
		INSERT INTO runs VALUES ('run-1', 'repo-1', 'feature', 'head', 'base', 'running', NULL, NULL, 1, 1);
		INSERT INTO step_results VALUES ('step-1', 'run-1', 'review', 1, 'running', NULL, NULL, NULL, NULL, NULL, 1, NULL, NULL, NULL, NULL, NULL);
	`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := db.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("open the pre-upgrade database read-only: %v", err)
	}
	defer readOnly.Close()

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	inspector := gatecontext.Inspector{DB: readOnly, Paths: p}
	if _, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: t.TempDir(), PeerPID: os.Getpid()}); err != nil {
		t.Fatalf("classification failed against a schema older than this binary, which would block every pipeline-control command: %v", err)
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
