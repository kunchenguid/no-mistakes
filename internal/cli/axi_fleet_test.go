package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fleetDoc is the machine-readable shape a dashboard parses out of
// `axi fleet` when at least one run is active.
type fleetDoc struct {
	Scope  string     `toon:"scope"`
	Daemon string     `toon:"daemon"`
	Count  string     `toon:"count"`
	Fleet  []fleetRow `toon:"fleet"`
	Help   []string   `toon:"help"`
}

// emptyFleetDoc omits the fleet key, which carries a plain sentence rather than
// a table when nothing is active.
type emptyFleetDoc struct {
	Scope  string   `toon:"scope"`
	Daemon string   `toon:"daemon"`
	Count  string   `toon:"count"`
	Help   []string `toon:"help"`
}

func axiFleetOutput(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if err := runAxiFleet(cmd); err != nil {
		t.Fatalf("axi fleet: %v\n%s", err, out.String())
	}
	return out.String()
}

func decodeFleetDoc(t *testing.T, out string) fleetDoc {
	t.Helper()
	var doc fleetDoc
	if err := toon.UnmarshalString(out, &doc); err != nil {
		t.Fatalf("decode axi fleet TOON: %v\n%s", err, out)
	}
	return doc
}

func fleetRowFor(t *testing.T, doc fleetDoc, runID string) fleetRow {
	t.Helper()
	for _, row := range doc.Fleet {
		if row.Run == runID {
			return row
		}
	}
	t.Fatalf("run %s missing from fleet view: %+v", runID, doc.Fleet)
	return fleetRow{}
}

// setupFleetHome points the CLI at an isolated NM_HOME with no registered
// repository, so each test registers exactly the repositories it needs.
func setupFleetHome(t *testing.T) (*paths.Paths, *db.DB) {
	t.Helper()
	return setupFleetHomeAt(t, t.TempDir())
}

// setupFleetHomeHostingAnEndpoint keeps NM_HOME under a short temp root, so a
// test that binds the daemon's IPC endpoint there fits the platform's socket
// path limit.
func setupFleetHomeHostingAnEndpoint(t *testing.T) (*paths.Paths, *db.DB) {
	t.Helper()
	return setupFleetHomeAt(t, makeSocketSafeTempDir(t))
}

func setupFleetHomeAt(t *testing.T, nmHome string) (*paths.Paths, *db.DB) {
	t.Helper()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return p, database
}

// registerGitRepo creates a real git repository under root/name and registers
// it, returning the resolved working path the fleet view must report.
func registerGitRepo(t *testing.T, database *db.DB, root, name, id string) (string, *db.Repo) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "test@test.com")
	run(t, dir, "git", "config", "user.name", "Test")
	run(t, dir, "git", "commit", "--allow-empty", "-m", "initial")
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolved = dir
	}
	repo, err := database.InsertRepoWithID(id, resolved, "origin", "main")
	if err != nil {
		t.Fatalf("register repo %s: %v", name, err)
	}
	return resolved, repo
}

func startedRun(t *testing.T, database *db.DB, repoID, branch, head string) *db.Run {
	t.Helper()
	r, err := database.InsertRun(repoID, branch, head, "base")
	if err != nil {
		t.Fatalf("insert run on %s: %v", branch, err)
	}
	if err := database.UpdateRunStatus(r.ID, types.RunRunning); err != nil {
		t.Fatalf("start run on %s: %v", branch, err)
	}
	return r
}

func runningStep(t *testing.T, database *db.DB, runID string, name types.StepName) *db.StepResult {
	t.Helper()
	step, err := database.InsertStepResult(runID, name)
	if err != nil {
		t.Fatalf("insert %s step: %v", name, err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatalf("start %s step: %v", name, err)
	}
	return step
}

// TestAxiFleetReportsEveryActiveRunAcrossRepositories is the core of the
// machine-wide view: one call from anywhere finds every live pipeline, with no
// list of project paths supplied by the caller, and terminal runs stay out of
// it.
func TestAxiFleetReportsEveryActiveRunAcrossRepositories(t *testing.T) {
	_, database := setupFleetHome(t)
	root := t.TempDir()
	alphaPath, alpha := registerGitRepo(t, database, root, "alpha", "repo-alpha")
	betaPath, beta := registerGitRepo(t, database, root, "beta", "repo-beta")

	first := startedRun(t, database, alpha.ID, "feature/one", "aaaaaaaaaaaa")
	second := startedRun(t, database, alpha.ID, "feature/two", "bbbbbbbbbbbb")
	runningStep(t, database, second.ID, types.StepCI)
	if err := database.SetRunCIReady(second.ID, true); err != nil {
		t.Fatalf("record CI readiness: %v", err)
	}
	third := startedRun(t, database, beta.ID, "feature/three", "cccccccccccc")
	runningStep(t, database, third.ID, types.StepCI)
	if err := database.UpdateRunPRURL(third.ID, "https://example.test/pr/7"); err != nil {
		t.Fatalf("record PR URL: %v", err)
	}

	finished := startedRun(t, database, beta.ID, "feature/done", "dddddddddddd")
	if err := database.UpdateRunStatus(finished.ID, types.RunCompleted); err != nil {
		t.Fatalf("complete run: %v", err)
	}

	// Answer from a directory that belongs to no registered repository: the
	// fleet view must not depend on the caller's location.
	chdir(t, t.TempDir())
	out := axiFleetOutput(t)
	doc := decodeFleetDoc(t, out)

	if doc.Scope != "machine" {
		t.Fatalf("scope = %q, want machine:\n%s", doc.Scope, out)
	}
	if len(doc.Fleet) != 3 {
		t.Fatalf("fleet listed %d runs, want the 3 active ones:\n%s", len(doc.Fleet), out)
	}
	if want := "3 active, 0 parked, in 2 of 2 repositories"; doc.Count != want {
		t.Fatalf("count = %q, want %q:\n%s", doc.Count, want, out)
	}
	if strings.Contains(out, finished.ID) {
		t.Fatalf("terminal run %s must not appear in the fleet view:\n%s", finished.ID, out)
	}

	firstRow := fleetRowFor(t, doc, first.ID)
	if firstRow.Repo != alphaPath || firstRow.Branch != "feature/one" || firstRow.Status != string(types.RunRunning) {
		t.Fatalf("run %s row = %+v, want repo %s branch feature/one running", first.ID, firstRow, alphaPath)
	}
	secondRow := fleetRowFor(t, doc, second.ID)
	if secondRow.Repo != alphaPath || secondRow.Branch != "feature/two" {
		t.Fatalf("second run row = %+v, want repo %s branch feature/two", secondRow, alphaPath)
	}
	// Recorded readiness is the only thing the checks column reports, and it
	// reports it while the CI step keeps monitoring the open PR.
	if secondRow.Stage != "ci:running" || secondRow.Checks != "passed" {
		t.Fatalf("second run row = %+v, want stage ci:running and checks passed", secondRow)
	}
	thirdRow := fleetRowFor(t, doc, third.ID)
	if thirdRow.Repo != betaPath || thirdRow.PR != "https://example.test/pr/7" {
		t.Fatalf("third run row = %+v, want repo %s with its PR URL", thirdRow, betaPath)
	}
	// A CI step that is running but has recorded no readiness yet is live
	// activity, which stage already carries; checks must not restate it.
	if thirdRow.Stage != "ci:running" || thirdRow.Checks != "" {
		t.Fatalf("third run row = %+v, want stage ci:running and empty checks", thirdRow)
	}

	// Rows are grouped by repository root so a dashboard renders them without
	// re-sorting.
	grouped := make([]string, 0, len(doc.Fleet))
	for _, row := range doc.Fleet {
		grouped = append(grouped, row.Repo)
	}
	sorted := append([]string(nil), grouped...)
	sort.Strings(sorted)
	if strings.Join(grouped, "\x00") != strings.Join(sorted, "\x00") {
		t.Fatalf("fleet rows are not grouped by repository root: %v", grouped)
	}
}

// TestAxiFleetIncludesAWorktreeOriginatedRun covers the case a machine-wide
// dashboard exists for: work launched from a linked worktree. The run is
// reported under the registered repository root, and the view answers the same
// way from inside that worktree.
func TestAxiFleetIncludesAWorktreeOriginatedRun(t *testing.T) {
	_, database := setupFleetHome(t)
	root := t.TempDir()
	mainPath, repo := registerGitRepo(t, database, root, "main", "repo-main")

	worktreeDir := filepath.Join(root, "wt")
	run(t, mainPath, "git", "worktree", "add", "-b", "feature/worktree", worktreeDir)

	fromWorktree := startedRun(t, database, repo.ID, "feature/worktree", "eeeeeeeeeeee")
	runningStep(t, database, fromWorktree.ID, types.StepReview)

	chdir(t, worktreeDir)
	doc := decodeFleetDoc(t, axiFleetOutput(t))
	row := fleetRowFor(t, doc, fromWorktree.ID)
	if row.Repo != mainPath {
		t.Fatalf("worktree-originated run reported repo %q, want the registered root %q", row.Repo, mainPath)
	}
	if row.Branch != "feature/worktree" || row.Stage != "review:running" {
		t.Fatalf("worktree-originated run row = %+v, want branch feature/worktree at review:running", row)
	}
	if row.Activity == "" {
		t.Fatalf("worktree-originated run row reported no recent activity: %+v", row)
	}
}

// TestAxiFleetWithNoActiveRunsReportsNone keeps the empty machine readable:
// registered repositories whose runs have all finished report zero active work
// and still succeed.
func TestAxiFleetWithNoActiveRunsReportsNone(t *testing.T) {
	_, database := setupFleetHome(t)
	root := t.TempDir()
	_, repo := registerGitRepo(t, database, root, "only", "repo-only")
	finished := startedRun(t, database, repo.ID, "feature/done", "ffffffffffff")
	if err := database.UpdateRunStatus(finished.ID, types.RunCompleted); err != nil {
		t.Fatalf("complete run: %v", err)
	}

	chdir(t, t.TempDir())
	out := axiFleetOutput(t)
	var doc emptyFleetDoc
	if err := toon.UnmarshalString(out, &doc); err != nil {
		t.Fatalf("decode axi fleet TOON: %v\n%s", err, out)
	}
	if want := "0 active, 0 parked, in 0 of 1 repositories"; doc.Count != want {
		t.Fatalf("count = %q, want %q:\n%s", doc.Count, want, out)
	}
	if !strings.Contains(out, "no active or parked runs on this machine") {
		t.Fatalf("empty fleet view must say so plainly:\n%s", out)
	}
	if strings.Contains(out, finished.ID) {
		t.Fatalf("completed run %s leaked into the empty fleet view:\n%s", finished.ID, out)
	}
}

// TestAxiFleetReportsAParkedDecision proves the view distinguishes a run
// waiting for its driving agent from one that is executing, and says how long
// it has waited.
func TestAxiFleetReportsAParkedDecision(t *testing.T) {
	_, database := setupFleetHome(t)
	root := t.TempDir()
	_, repo := registerGitRepo(t, database, root, "parked", "repo-parked")

	parked := startedRun(t, database, repo.ID, "feature/parked", "111111111111")
	step := runningStep(t, database, parked.ID, types.StepReview)
	findings := `{"summary":"one blocking finding","items":[{"id":"F1","severity":"high","action":"ask-user","description":"needs a decision"}]}`
	if err := database.ParkStepForApproval(parked.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1200, &findings); err != nil {
		t.Fatalf("park step: %v", err)
	}
	stored, err := database.GetRun(parked.ID)
	if err != nil || stored.AwaitingAgentSince == nil {
		t.Fatalf("run did not record the park: %v %+v", err, stored)
	}
	pinned := *stored.AwaitingAgentSince + 125
	previous := nowUnix
	nowUnix = func() int64 { return pinned }
	t.Cleanup(func() { nowUnix = previous })

	running := startedRun(t, database, repo.ID, "feature/running", "222222222222")
	runningStep(t, database, running.ID, types.StepTest)

	chdir(t, t.TempDir())
	out := axiFleetOutput(t)
	doc := decodeFleetDoc(t, out)

	parkedRow := fleetRowFor(t, doc, parked.ID)
	if parkedRow.Stage != "review:awaiting_approval" {
		t.Fatalf("parked run stage = %q, want review:awaiting_approval:\n%s", parkedRow.Stage, out)
	}
	if parkedRow.Activity != "parked 2m5s" {
		t.Fatalf("parked run activity = %q, want parked 2m5s:\n%s", parkedRow.Activity, out)
	}
	runningRow := fleetRowFor(t, doc, running.ID)
	if runningRow.Stage != "test:running" || strings.HasPrefix(runningRow.Activity, "parked") {
		t.Fatalf("executing run row = %+v, want test:running and no parked activity", runningRow)
	}
	if want := "2 active, 1 parked, in 1 of 1 repositories"; doc.Count != want {
		t.Fatalf("count = %q, want %q:\n%s", doc.Count, want, out)
	}
	if !strings.Contains(strings.Join(doc.Help, "\n"), "axi respond") {
		t.Fatalf("a parked fleet view must point at the response command:\n%s", out)
	}
}

// TestAxiFleetIsReadOnly is the guarantee the command exists under: observing
// the fleet must not start, answer, or otherwise disturb any pipeline, and must
// not bring the daemon up.
func TestAxiFleetIsReadOnly(t *testing.T) {
	p, database := setupFleetHome(t)
	root := t.TempDir()
	_, repo := registerGitRepo(t, database, root, "quiet", "repo-quiet")

	active := startedRun(t, database, repo.ID, "feature/active", "333333333333")
	step := runningStep(t, database, active.ID, types.StepReview)
	findings := `{"summary":"decide","items":[{"id":"F1","severity":"high","action":"ask-user","description":"needs a decision"}]}`
	if err := database.ParkStepForApproval(active.ID, step.ID, types.StepStatusAwaitingApproval, 0, 900, &findings); err != nil {
		t.Fatalf("park step: %v", err)
	}

	before := runSnapshot(t, database, active.ID)
	chdir(t, t.TempDir())
	_ = axiFleetOutput(t)
	_ = axiFleetOutput(t)
	after := runSnapshot(t, database, active.ID)

	if before != after {
		t.Fatalf("axi fleet changed pipeline state:\nbefore: %s\nafter:  %s", before, after)
	}
	if _, err := os.Stat(p.Socket()); err == nil {
		t.Fatalf("axi fleet started the daemon: socket %s exists", p.Socket())
	}
}

// TestAxiFleetDaemonStateFollowsTheProbe covers the degraded case a
// machine-wide observer must not be misled by. The same live endpoint is
// probed twice: while it answers the health call the view reports a running
// daemon, and once it accepts the connection but stops answering the view must
// report the state as unknown - surfacing the probe failure instead of
// claiming a stopped daemon whose rows are merely the last persisted state.
func TestAxiFleetDaemonStateFollowsTheProbe(t *testing.T) {
	p, database := setupFleetHomeHostingAnEndpoint(t)
	root := t.TempDir()
	_, repo := registerGitRepo(t, database, root, "stuck", "repo-stuck")
	active := startedRun(t, database, repo.ID, "feature/stuck", "444444444444")
	runningStep(t, database, active.ID, types.StepReview)

	answering := serveHealth(t, p)
	waitForDaemonRunning(t, p)
	chdir(t, t.TempDir())

	runningOut := axiFleetOutput(t)
	runningDoc := decodeFleetDoc(t, runningOut)
	if runningDoc.Daemon != "running" {
		t.Fatalf("daemon = %q while the endpoint answers health, want running:\n%s", runningDoc.Daemon, runningOut)
	}

	answering.Store(false)
	out := axiFleetOutput(t)
	doc := decodeFleetDoc(t, out)

	if doc.Daemon != "unknown" {
		t.Fatalf("daemon = %q with an endpoint that never answers, want unknown:\n%s", doc.Daemon, out)
	}
	help := strings.Join(doc.Help, "\n")
	if !strings.Contains(help, "probe did not conclude") || !strings.Contains(help, "did not reply") {
		t.Fatalf("an unprobable daemon must be reported as such, with its probe error:\n%s", out)
	}
	if strings.Contains(help, "last persisted state") {
		t.Fatalf("an unprobable daemon must not be reported as a stopped one:\n%s", out)
	}
	// The rows are the persisted state either way, and stay readable.
	if row := fleetRowFor(t, doc, active.ID); row.Stage != "review:running" {
		t.Fatalf("active run row = %+v, want review:running", row)
	}
}

// TestAxiFleetReportsACrashedDaemonAsStopped is the other half of the same
// distinction: a daemon that dies without cleanup leaves its endpoint on disk,
// and nothing answers there. That is a conclusion, not a failed probe, so it
// must still read as a stopped daemon whose rows are the last persisted state.
func TestAxiFleetReportsACrashedDaemonAsStopped(t *testing.T) {
	p, database := setupFleetHomeHostingAnEndpoint(t)
	root := t.TempDir()
	_, repo := registerGitRepo(t, database, root, "crashed", "repo-crashed")
	active := startedRun(t, database, repo.ID, "feature/crashed", "555555555555")
	runningStep(t, database, active.ID, types.StepReview)

	if err := os.WriteFile(p.Socket(), []byte("leftover endpoint from a killed daemon"), 0o600); err != nil {
		t.Fatalf("leave a stale endpoint: %v", err)
	}

	chdir(t, t.TempDir())
	out := axiFleetOutput(t)
	doc := decodeFleetDoc(t, out)

	if doc.Daemon != "stopped" {
		t.Fatalf("daemon = %q with a leftover endpoint nothing answers, want stopped:\n%s", doc.Daemon, out)
	}
	if !strings.Contains(strings.Join(doc.Help, "\n"), "last persisted state") {
		t.Fatalf("a stopped-daemon fleet view must label its rows as persisted state:\n%s", out)
	}
	if row := fleetRowFor(t, doc, active.ID); row.Stage != "review:running" {
		t.Fatalf("active run row = %+v, want review:running", row)
	}
}

// serveHealth serves the real IPC health method on the daemon's endpoint. The
// returned switch decides whether a probe is answered: turned off, the
// connection is still accepted and then left hanging, which is what a live but
// wedged daemon looks like to the probe.
func serveHealth(t *testing.T, p *paths.Paths) *atomic.Bool {
	t.Helper()
	answering := &atomic.Bool{}
	answering.Store(true)
	blocked := make(chan struct{})

	server := ipc.NewServer()
	server.Handle(ipc.MethodHealth, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		if answering.Load() {
			return &ipc.HealthResult{Status: "ok"}, nil
		}
		select {
		case <-blocked:
		case <-ctx.Done():
		}
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	if err := server.Listen(p.Socket()); err != nil {
		t.Fatalf("listen on the daemon endpoint: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- server.ServeReady() }()
	t.Cleanup(func() {
		close(blocked)
		server.Close()
		<-served
	})
	return answering
}

// runSnapshot serializes the state a fleet read must leave untouched.
func runSnapshot(t *testing.T, database *db.DB, runID string) string {
	t.Helper()
	r, err := database.GetRun(runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	var b strings.Builder
	awaiting := int64(0)
	if r.AwaitingAgentSince != nil {
		awaiting = *r.AwaitingAgentSince
	}
	fmt.Fprintf(&b, "run=%s status=%s head=%s awaiting=%d updated=%d", r.ID, r.Status, r.HeadSHA, awaiting, r.UpdatedAt)
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	for _, s := range steps {
		findings := ""
		if s.FindingsJSON != nil {
			findings = *s.FindingsJSON
		}
		fmt.Fprintf(&b, " | step=%s status=%s findings=%s", s.StepName, s.Status, findings)
	}
	return b.String()
}
