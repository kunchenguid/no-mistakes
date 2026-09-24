//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fleetViewRow is the dashboard's view of one `axi fleet` row. The emitted
// TOON document is the machine-readable interface this command exists to
// serve, so the journey consumes it the way an observer would rather than
// matching on prose.
type fleetViewRow struct {
	Repo     string `toon:"repo"`
	Branch   string `toon:"branch"`
	Run      string `toon:"run"`
	Status   string `toon:"status"`
	Stage    string `toon:"stage"`
	Activity string `toon:"activity"`
	PR       string `toon:"pr"`
	Checks   string `toon:"checks"`
}

type fleetView struct {
	Scope  string         `toon:"scope"`
	Daemon string         `toon:"daemon"`
	Count  string         `toon:"count"`
	Fleet  []fleetViewRow `toon:"fleet"`
	Help   []string       `toon:"help"`
}

// emptyFleetView mirrors the document shape when nothing is active: `fleet`
// carries a sentence instead of a table.
type emptyFleetView struct {
	Scope  string   `toon:"scope"`
	Daemon string   `toon:"daemon"`
	Count  string   `toon:"count"`
	Fleet  string   `toon:"fleet"`
	Help   []string `toon:"help"`
}

// fleetGateScenario parks every pipeline at the review gate with one ask-user
// finding, which is the state a machine-wide observer most needs to see: work
// that is alive and waiting for its driving agent.
func fleetGateScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review found a warning"
    structured:
      findings:
        - id: "fleet-1"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "needs a human decision"
          action: ask-user
      summary: "found 1 issue"
      risk_level: medium
      risk_rationale: "warning requires human review"
      risk_scope: source-or-external
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fleet scenario: %v", err)
	}
	return path
}

// TestAxiFleetMachineWideJourney drives the machine-wide view the way a
// standalone dashboard would: two independent repositories on one machine,
// each with a live pipeline, discovered in a single call from a directory that
// belongs to no repository and with no list of project paths supplied. It also
// drives the boundaries the view promises - terminal runs stay out, the read
// cannot mutate a run or start the daemon, a stopped daemon still answers from
// persisted state, and the surface emits no telemetry.
func TestAxiFleetMachineWideJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: fleetGateScenario(t)})

	// A directory that belongs to no registered repository: the whole point is
	// that the observer needs neither a repository nor a path list.
	outside := t.TempDir()

	emptyOut := fleetOutput(t, h, outside)
	t.Logf("=== FLEET TRANSCRIPT: empty machine, from a non-repository directory ===\n%s", emptyOut)
	var empty emptyFleetView
	if err := toon.UnmarshalString(emptyOut, &empty); err != nil {
		t.Fatalf("decode empty fleet document: %v\n%s", err, emptyOut)
	}
	if empty.Scope != "machine" {
		t.Fatalf("scope = %q, want machine:\n%s", empty.Scope, emptyOut)
	}
	if empty.Fleet != "no active or parked runs on this machine" {
		t.Fatalf("empty fleet = %q, want the plain sentence:\n%s", empty.Fleet, emptyOut)
	}
	if empty.Count != "0 active, 0 parked, in 0 of 0 repositories" {
		t.Fatalf("empty count = %q:\n%s", empty.Count, emptyOut)
	}

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init first repository: %v\n%s", err, out)
	}
	secondWork := registerSecondRepository(t, h, "beta")

	firstRoot := resolvedPath(t, h.WorkDir)
	secondRoot := resolvedPath(t, secondWork)

	// First repository: the run is launched from a linked worktree, the case a
	// machine-wide dashboard exists for. Its row must name the registered
	// repository root, not the worktree it was launched from.
	h.CommitChange("feature/alpha", "feature.txt", "alpha change\n", "add alpha feature")
	alphaWorktree := h.AddWorktree("feature/alpha")
	alphaGate, err := h.RunInDir(alphaWorktree, "axi", "run", "--intent", "ship the alpha feature")
	if err != nil || !strings.Contains(alphaGate, "fleet-1") {
		t.Fatalf("alpha run did not park at the review gate: %v\n%s", err, alphaGate)
	}

	// Second repository: launched from its own clone root, with a different
	// branch name so the rows are unambiguous.
	commitOnBranch(t, h, secondWork, "feature/beta", "feature.txt", "beta change\n", "add beta feature")
	betaGate, err := h.RunInDir(secondWork, "axi", "run", "--intent", "ship the beta feature")
	if err != nil || !strings.Contains(betaGate, "fleet-1") {
		t.Fatalf("beta run did not park at the review gate: %v\n%s", err, betaGate)
	}

	alphaRun := activeRunID(t, h, alphaWorktree)
	betaRun := activeRunID(t, h, secondWork)
	if alphaRun == "" || betaRun == "" || alphaRun == betaRun {
		t.Fatalf("expected two distinct active runs, got alpha=%q beta=%q", alphaRun, betaRun)
	}

	before := fleetStateSnapshot(t, h, outside)
	fleetOut := fleetOutput(t, h, outside)
	t.Logf("=== FLEET TRANSCRIPT: two repositories live, from a non-repository directory ===\n%s", fleetOut)
	view := decodeFleetView(t, fleetOut)

	if view.Scope != "machine" || view.Daemon != "running" {
		t.Fatalf("scope/daemon = %q/%q, want machine/running:\n%s", view.Scope, view.Daemon, fleetOut)
	}
	if len(view.Fleet) != 2 {
		t.Fatalf("fleet listed %d runs, want both live pipelines:\n%s", len(view.Fleet), fleetOut)
	}
	if want := "2 active, 2 parked, in 2 of 2 repositories"; view.Count != want {
		t.Fatalf("count = %q, want %q:\n%s", view.Count, want, fleetOut)
	}

	alphaRow := fleetRowByRun(t, view, alphaRun)
	if alphaRow.Repo != firstRoot {
		t.Fatalf("worktree-launched run reported repo %q, want the registered root %q", alphaRow.Repo, firstRoot)
	}
	if alphaRow.Branch != "feature/alpha" || alphaRow.Status != string(types.RunRunning) {
		t.Fatalf("alpha row = %+v, want branch feature/alpha running", alphaRow)
	}
	betaRow := fleetRowByRun(t, view, betaRun)
	if betaRow.Repo != secondRoot || betaRow.Branch != "feature/beta" {
		t.Fatalf("beta row = %+v, want repo %q branch feature/beta", betaRow, secondRoot)
	}
	for _, row := range []fleetViewRow{alphaRow, betaRow} {
		if row.Stage != "review:awaiting_approval" {
			t.Fatalf("row %+v: stage = %q, want review:awaiting_approval", row, row.Stage)
		}
		if !strings.HasPrefix(row.Activity, "parked ") {
			t.Fatalf("row %+v: activity = %q, want a parked duration", row, row.Activity)
		}
		// Neither run has reached CI, so nothing may claim a check outcome.
		if row.Checks != "" || row.PR != "" {
			t.Fatalf("row %+v: a run parked at review must report no checks and no PR", row)
		}
	}
	if !strings.Contains(strings.Join(view.Help, "\n"), "axi respond") {
		t.Fatalf("a parked fleet view must point at the response command:\n%s", fleetOut)
	}

	// Reading the fleet twice, and from inside one of the repositories'
	// worktrees, must produce the same machine-wide answer and leave every
	// pipeline exactly as it was.
	fromWorktree := decodeFleetView(t, fleetOutput(t, h, alphaWorktree))
	if len(fromWorktree.Fleet) != 2 || fromWorktree.Count != view.Count {
		t.Fatalf("fleet view read from inside a repository is not machine-wide: %+v", fromWorktree)
	}
	if row := fleetRowByRun(t, fromWorktree, alphaRun); row.Repo != firstRoot {
		t.Fatalf("row read from inside the worktree reported repo %q, want %q", row.Repo, firstRoot)
	}
	if after := fleetStateSnapshot(t, h, outside); after != before {
		t.Fatalf("reading the fleet changed pipeline state:\nbefore: %s\nafter:  %s", before, after)
	}

	// The view is a read, so it must not accept a mutating argument at all.
	if out, err := h.RunInDir(outside, "axi", "fleet", "respond"); err == nil {
		t.Fatalf("axi fleet accepted an extra argument instead of refusing it:\n%s", out)
	}

	// The machine-wide view shares the environment setup with every other axi
	// query, so prove the repository-scoped surfaces did not widen with it:
	// each answers for its own repository only, and the home view still
	// requires one.
	betaStatus, err := h.RunInDir(secondWork, "axi", "status")
	if err != nil {
		t.Fatalf("axi status in the second repository: %v\n%s", err, betaStatus)
	}
	if !strings.Contains(betaStatus, betaRun) || strings.Contains(betaStatus, alphaRun) {
		t.Fatalf("axi status leaked another repository's run:\n%s", betaStatus)
	}
	alphaHome, err := h.RunInDir(alphaWorktree, "axi")
	if err != nil {
		t.Fatalf("axi home in the first repository: %v\n%s", err, alphaHome)
	}
	if !strings.Contains(alphaHome, alphaRun) || strings.Contains(alphaHome, betaRun) {
		t.Fatalf("axi home leaked another repository's run:\n%s", alphaHome)
	}
	if out, err := h.RunInDir(outside, "axi"); err == nil {
		t.Fatalf("axi home answered outside any repository:\n%s", out)
	}

	// Telemetry: this read-only surface must send nothing even with telemetry
	// switched on, while a tracked command still reports - otherwise a silent
	// collector would "prove" the same thing.
	var received int64
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	telemetryEnv := map[string]string{
		"NO_MISTAKES_TELEMETRY":        "on",
		"NO_MISTAKES_UMAMI_HOST":       collector.URL,
		"NO_MISTAKES_UMAMI_WEBSITE_ID": "fleet-journey",
	}
	if out, err := h.RunInDirWithEnv(outside, telemetryEnv, "axi", "fleet"); err != nil {
		t.Fatalf("axi fleet with telemetry enabled: %v\n%s", err, out)
	}
	if got := atomic.LoadInt64(&received); got != 0 {
		t.Fatalf("axi fleet sent %d telemetry requests, want none", got)
	}
	if out, err := h.RunInDirWithEnv(h.WorkDir, telemetryEnv, "daemon", "status"); err != nil {
		t.Fatalf("control tracked command: %v\n%s", err, out)
	}
	if got := atomic.LoadInt64(&received); got == 0 {
		t.Fatal("the telemetry collector saw nothing at all, so the fleet result proves nothing")
	}

	// A run that finishes leaves the fleet: the view reports live work, never a
	// growing history.
	if out, err := h.RunInDir(alphaWorktree, "axi", "respond", "--action", "approve"); err != nil {
		t.Fatalf("approve the alpha review gate: %v\n%s", err, out)
	}
	finished := h.WaitForRun("feature/alpha", 120*time.Second)
	if !finished.Status.Terminal() {
		t.Fatalf("alpha run did not reach a terminal status: %s", finished.Status)
	}
	afterOut := fleetOutput(t, h, outside)
	t.Logf("=== FLEET TRANSCRIPT: after the alpha pipeline finished ===\n%s", afterOut)
	afterView := decodeFleetView(t, afterOut)
	if strings.Contains(afterOut, alphaRun) {
		t.Fatalf("terminal run %s is still listed in the fleet view:\n%s", alphaRun, afterOut)
	}
	if len(afterView.Fleet) != 1 || afterView.Fleet[0].Run != betaRun {
		t.Fatalf("fleet after completion = %+v, want only the still-parked beta run", afterView.Fleet)
	}
	if want := "1 active, 1 parked, in 1 of 2 repositories"; afterView.Count != want {
		t.Fatalf("count = %q, want %q:\n%s", afterView.Count, want, afterOut)
	}

	// A daemon that dies without reconciling (crash, kill, host restart) is the
	// case where "the rows are the last persisted state" has to hold: the beta
	// run is still parked, and the observer must both see it and be told the
	// rows are stale rather than live.
	killDaemon(t, h)
	stoppedOut := fleetOutput(t, h, outside)
	t.Logf("=== FLEET TRANSCRIPT: daemon gone, killed without reconciling ===\n%s", stoppedOut)
	stoppedView := decodeFleetView(t, stoppedOut)
	if stoppedView.Daemon != "stopped" {
		t.Fatalf("daemon = %q with the daemon stopped:\n%s", stoppedView.Daemon, stoppedOut)
	}
	if len(stoppedView.Fleet) != 1 || stoppedView.Fleet[0].Run != betaRun {
		t.Fatalf("stopped-daemon fleet = %+v, want the persisted beta run", stoppedView.Fleet)
	}
	if !strings.Contains(strings.Join(stoppedView.Help, "\n"), "last persisted state") {
		t.Fatalf("a stopped-daemon fleet view must label its rows as persisted state:\n%s", stoppedOut)
	}
	if alive, err := daemon.IsRunning(paths.WithRoot(h.NMHome)); err == nil && alive {
		t.Fatal("axi fleet started the daemon")
	}
	assertNoDaemonProcessesForRoot(t, h, "after axi fleet with the daemon stopped")
}

// killDaemon takes the daemon down the way a crash or a host restart does,
// without the graceful shutdown that reconciles active runs.
func killDaemon(t *testing.T, h *Harness) {
	t.Helper()
	pid := daemonPIDForRoot(t, h)
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find daemon process %d: %v", pid, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill daemon process %d: %v", pid, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		alive, err := e2edaemon.ProcessAlive(pid)
		if err != nil {
			t.Fatalf("probe killed daemon %d: %v", pid, err)
		}
		if !alive {
			assertNoDaemonProcessesForRoot(t, h, "after killing the daemon")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon process %d survived SIGKILL", pid)
}

func fleetOutput(t *testing.T, h *Harness, dir string) string {
	t.Helper()
	out, err := h.RunInDir(dir, "axi", "fleet")
	if err != nil {
		t.Fatalf("axi fleet in %s: %v\n%s", dir, err, out)
	}
	return out
}

func decodeFleetView(t *testing.T, out string) fleetView {
	t.Helper()
	var view fleetView
	if err := toon.UnmarshalString(out, &view); err != nil {
		t.Fatalf("decode fleet document: %v\n%s", err, out)
	}
	return view
}

func fleetRowByRun(t *testing.T, view fleetView, runID string) fleetViewRow {
	t.Helper()
	for _, row := range view.Fleet {
		if row.Run == runID {
			return row
		}
	}
	t.Fatalf("run %s missing from the fleet view: %+v", runID, view.Fleet)
	return fleetViewRow{}
}

// fleetStateSnapshot serializes what a machine-wide read must leave untouched:
// every listed run's status, stage, and parked activity.
func fleetStateSnapshot(t *testing.T, h *Harness, dir string) string {
	t.Helper()
	view := decodeFleetView(t, fleetOutput(t, h, dir))
	parts := make([]string, 0, len(view.Fleet))
	for _, row := range view.Fleet {
		parts = append(parts, row.Run+"/"+row.Status+"/"+row.Stage+"/"+row.PR+"/"+row.Checks)
	}
	return strings.Join(parts, " | ")
}

// activeRunID asks the read-only repository-scoped view for the run ID of the
// pipeline active on the caller's branch, so the journey can match fleet rows
// to runs without reading the database.
func activeRunID(t *testing.T, h *Harness, dir string) string {
	t.Helper()
	out, err := h.RunInDir(dir, "axi", "status")
	if err != nil {
		t.Fatalf("axi status in %s: %v\n%s", dir, err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "id:"); ok {
			return strings.Trim(strings.TrimSpace(id), `"`)
		}
	}
	t.Fatalf("no run id in axi status output:\n%s", out)
	return ""
}

// registerSecondRepository creates a second real repository (bare origin plus
// working clone) beside the harness one and registers it with the same daemon.
// Two independent repositories on one machine is what makes the machine-wide
// claim testable at all.
func registerSecondRepository(t *testing.T, h *Harness, name string) string {
	t.Helper()
	ctx := context.Background()
	mustGit := func(dir string, args ...string) {
		if out, err := h.runGit(ctx, dir, args...); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	root := filepath.Dir(h.WorkDir)
	upstream := filepath.Join(root, name+"-upstream.git")
	work := filepath.Join(root, name)
	for _, dir := range []string{upstream, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	mustGit(upstream, "init", "--bare", "--initial-branch=main")
	mustGit(work, "init", "--initial-branch=main")
	mustGit(work, "config", "user.email", "e2e@example.com")
	mustGit(work, "config", "user.name", "E2E Test")
	mustGit(work, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte("allow_repo_commands: true\n"), 0o644); err != nil {
		t.Fatalf("write repo config: %v", err)
	}
	mustGit(work, "add", "README.md", ".no-mistakes.yaml")
	mustGit(work, "commit", "-m", "initial commit")
	mustGit(work, "remote", "add", "origin", upstream)
	mustGit(work, "push", "-u", "origin", "main")
	if out, err := h.RunInDir(work, "init"); err != nil {
		t.Fatalf("init repository %s: %v\n%s", name, err, out)
	}
	return work
}

func commitOnBranch(t *testing.T, h *Harness, dir, branch, path, content, message string) {
	t.Helper()
	ctx := context.Background()
	mustGit := func(args ...string) {
		if out, err := h.runGit(ctx, dir, args...); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	mustGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	mustGit("add", path)
	mustGit("commit", "-m", message)
}

func resolvedPath(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	return resolved
}

// TestAxiFleetReportsPublishedPRAndCheckReadiness drives a real pipeline past
// publication and into CI monitoring, so the fleet view's `pr` and `checks`
// columns are filled by the pipeline itself rather than by the observer. The
// repository declares `no_ci: true` on its trusted default branch, which is
// the one case where an empty forge result is recorded as readiness, so the
// run stays alive at `ci:running` with a published PR while the fleet is read.
func TestAxiFleetReportsPublishedPRAndCheckReadiness(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})

	const (
		giteaHost  = "gitea.example.com"
		repoSlug   = "owner/repo"
		loginName  = "e2e"
		remoteURL  = "https://" + giteaHost + "/" + repoSlug + ".git"
		branchName = "feature/fleet-ci"
	)

	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remoteURL); err != nil {
		t.Fatalf("set forge origin: %v\n%s", err, out)
	}
	configureTeaLogin(t, h, giteaHost, loginName)
	t.Setenv("FAKEAGENT_TEA_HOST", giteaHost)
	// The PR stays open, so the CI step keeps monitoring instead of finishing
	// the run the moment it observes a merge.
	t.Setenv("FAKEAGENT_TEA_PR_STATE", "open")
	declareNoCIOnDefaultBranch(t, h)

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	outside := t.TempDir()
	h.CommitChange(branchName, "fleet-ci.txt", "fleet ci change\n", "add fleet ci file")
	h.PushToGate(branchName)

	row := waitForFleetRow(t, h, outside, branchName, 120*time.Second, func(row fleetViewRow) bool {
		return row.Checks != ""
	})
	t.Logf("=== FLEET TRANSCRIPT: a published run under CI monitoring ===\n%s", fleetOutput(t, h, outside))

	if row.Repo != resolvedPath(t, h.WorkDir) {
		t.Fatalf("row %+v: repo = %q, want the registered root", row, row.Repo)
	}
	if row.Status != string(types.RunRunning) || row.Stage != "ci:running" {
		t.Fatalf("row %+v: want a running run at ci:running", row)
	}
	// The PR column must carry what the pipeline actually published.
	wantPRPrefix := "http://" + giteaHost + "/" + repoSlug + "/pulls/"
	if !strings.HasPrefix(row.PR, wantPRPrefix) {
		t.Fatalf("row %+v: pr = %q, want a published PR URL under %s", row, row.PR, wantPRPrefix)
	}
	// The repository declared it has no CI, so that is what the checks column
	// reports - never a plain "passed" that would claim checks actually ran.
	if row.Checks != "no-ci" {
		t.Fatalf("row %+v: checks = %q, want no-ci for a trusted no_ci declaration", row, row.Checks)
	}
	// A parked-for-a-decision row is what `activity` reports as a duration;
	// this run is working, so it reports the CI step's own recorded activity.
	if strings.HasPrefix(row.Activity, "parked ") || row.Activity == "" {
		t.Fatalf("row %+v: activity = %q, want the CI step's recorded activity", row, row.Activity)
	}

	if out, err := h.Run("axi", "abort"); err != nil {
		t.Fatalf("abort the monitored run: %v\n%s", err, out)
	}
}

// waitForFleetRow polls the machine-wide view the way a dashboard would, until
// the row for branch satisfies ready.
func waitForFleetRow(t *testing.T, h *Harness, dir, branch string, timeout time.Duration, ready func(fleetViewRow) bool) fleetViewRow {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = fleetOutput(t, h, dir)
		for _, row := range decodeFleetView(t, last).Fleet {
			if row.Branch == branch && ready(row) {
				return row
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no fleet row for %s reached the expected state within %s; last view:\n%s", branch, timeout, last)
	return fleetViewRow{}
}

// configureTeaLogin writes the tea config the Gitea provider is detected
// through, in an isolated XDG_CONFIG_HOME so an ambient one cannot leak in.
func configureTeaLogin(t *testing.T, h *Harness, host, login string) {
	t.Helper()
	xdgConfigHome := filepath.Join(h.HomeDir, ".config")
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	teaConfigDir := filepath.Join(xdgConfigHome, "tea")
	if err := os.MkdirAll(teaConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir tea config dir: %v", err)
	}
	teaConfig := "logins:\n" +
		"    - name: " + login + "\n" +
		"      url: https://" + host + "\n" +
		"      ssh_host: " + host + "\n" +
		"      user: e2e-tea-user\n" +
		"      token: xxx\n"
	if err := os.WriteFile(filepath.Join(teaConfigDir, "config.yml"), []byte(teaConfig), 0o644); err != nil {
		t.Fatalf("write tea config: %v", err)
	}
}

// declareNoCIOnDefaultBranch commits the trusted `no_ci: true` declaration to
// the default branch, which is the only place the CI step accepts it from.
func declareNoCIOnDefaultBranch(t *testing.T, h *Harness) {
	t.Helper()
	ctx := context.Background()
	configPath := filepath.Join(h.WorkDir, ".no-mistakes.yaml")
	existing, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read repo config: %v", err)
	}
	if err := os.WriteFile(configPath, append(existing, []byte("no_ci: true\n")...), 0o644); err != nil {
		t.Fatalf("write repo config: %v", err)
	}
	for _, args := range [][]string{
		{"add", ".no-mistakes.yaml"},
		{"commit", "-m", "declare that this repository has no CI"},
		{"push", "origin", "main"},
	} {
		if out, err := h.runGit(ctx, h.WorkDir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
