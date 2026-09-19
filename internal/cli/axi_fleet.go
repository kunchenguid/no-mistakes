package cli

import (
	"fmt"
	"sort"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

// fleetRow is one active or parked run anywhere on this machine. The columns
// are what a dashboard needs to place a run without opening it: where it lives,
// what it is doing, how recently it moved, and what it has published so far.
type fleetRow struct {
	Repo     string `toon:"repo"`
	Branch   string `toon:"branch"`
	Run      string `toon:"run"`
	Status   string `toon:"status"`
	Stage    string `toon:"stage"`
	Activity string `toon:"activity"`
	PR       string `toon:"pr"`
	Checks   string `toon:"checks"`
}

func newAxiFleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "List every active or parked run on this machine, across all repositories",
		Long: "Reports every pending, running, or parked pipeline run the local daemon\n" +
			"knows about, in every registered repository, so an observer discovers active\n" +
			"pipelines without a maintained list of project paths. A run launched from a\n" +
			"linked worktree is reported under its registered repository root.\n\n" +
			"This view is read-only. It never starts, answers, aborts, reruns, or\n" +
			"synchronizes a run, and it does not start the daemon; repository-scoped\n" +
			"commands such as `axi status` are unaffected by it.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAxiFleet(cmd)
		},
	}
	return cmd
}

// runAxiFleet renders the machine-wide view of active runs. Like the other
// read-only AXI queries it answers from the local database, so it works whether
// or not the daemon is up, needs no current repository, and emits no telemetry.
func runAxiFleet(cmd *cobra.Command) error {
	env, err := openAxiFleetEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	defer env.close()

	daemonState, probeErr := fleetDaemonState(env.p)

	repos, err := env.d.GetRepos()
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("list repositories: %v", err))
	}
	runs, err := env.d.GetActiveRuns()
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("list active runs: %v", err))
	}
	rows, reposWithRuns, err := fleetRows(env, repos, runs)
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}

	parked := 0
	for _, run := range runs {
		if run.AwaitingAgentSince != nil {
			parked++
		}
	}

	fields := []toon.Field{
		{Key: "scope", Value: "machine"},
		{Key: "daemon", Value: daemonState},
		{Key: "count", Value: fmt.Sprintf("%d active, %d parked, in %d of %d repositories", len(rows), parked, reposWithRuns, len(repos))},
	}
	if len(rows) == 0 {
		fields = append(fields, toon.Field{Key: "fleet", Value: "no active or parked runs on this machine"})
	} else {
		fields = append(fields, toon.Field{Key: "fleet", Value: rows})
	}
	fields = append(fields, toon.Field{Key: "help", Value: fleetHelp(daemonState, probeErr, parked)})

	emitDoc(cmd, fields...)
	return nil
}

// fleetDaemonState reports what the health probe actually established. A probe
// that timed out established nothing: the endpoint is there, taking the dial or
// the health call without answering it, so the daemon may well be live and
// still moving these runs. Calling that stopped would tell a machine-wide
// observer the rows are merely the last persisted state and swallow the probe
// error, so it is reported as unknown carrying that error instead. Every other
// outcome is a conclusion, including the refused dial a crashed daemon's
// leftover endpoint gives: nothing is serving it.
func fleetDaemonState(p *paths.Paths) (state string, probeErr string) {
	alive, err := daemon.IsRunning(p)
	switch {
	case alive:
		return "running", ""
	case ipc.IsConnectTimeout(err), ipc.IsCallTimeout(err):
		return "unknown", err.Error()
	default:
		return "stopped", ""
	}
}

func fleetHelp(daemonState, probeErr string, parked int) []string {
	help := []string{
		"This view is read-only and machine-wide: it never starts, answers, aborts, reruns, or synchronizes a run",
		"Run `no-mistakes axi status --run <id>` to inspect one listed run in detail; that selection is inspection-only",
	}
	if parked > 0 {
		help = append(help, "A parked run is waiting for its own driving agent, not stalled; answer its gate with `no-mistakes axi respond` from a worktree on that run's branch")
	}
	switch daemonState {
	case "stopped":
		help = append(help, "The daemon is not running, so these rows are the last persisted state; it reconciles runs it no longer owns when it next starts")
	case "unknown":
		help = append(help, "The daemon health probe did not conclude, so whether these rows are live or stale is unknown: "+probeErr)
	}
	return help
}

// fleetRows builds one row per active run, newest-first within each repository
// root so a dashboard can group by repository without re-sorting. It also
// returns how many distinct repositories are represented.
func fleetRows(env *axiEnv, repos []*db.Repo, runs []*db.Run) ([]fleetRow, int, error) {
	// A run launched from a linked worktree resolves to its main repository
	// when the run is recorded, so this root is the one a dashboard groups by.
	rootByRepoID := make(map[string]string, len(repos))
	for _, repo := range repos {
		rootByRepoID[repo.ID] = repo.WorkingPath
	}
	// GetActiveRuns already orders newest-first; a stable sort by repository
	// root keeps that order inside each group.
	ordered := make([]*db.Run, len(runs))
	copy(ordered, runs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return rootByRepoID[ordered[i].RepoID] < rootByRepoID[ordered[j].RepoID]
	})

	rows := make([]fleetRow, 0, len(ordered))
	repoIDs := make(map[string]struct{}, len(ordered))
	for _, run := range ordered {
		steps, err := env.d.GetStepsByRun(run.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("load steps for run %s: %w", run.ID, err)
		}
		rv := runViewFromDB(run, steps, env.d)
		annotateRunView(env, &rv)
		rows = append(rows, fleetRow{
			Repo:     rootByRepoID[run.RepoID],
			Branch:   run.Branch,
			Run:      run.ID,
			Status:   rv.Status,
			Stage:    fleetStage(rv),
			Activity: fleetActivity(rv),
			PR:       rv.PRURL,
			Checks:   fleetChecks(run),
		})
		repoIDs[run.RepoID] = struct{}{}
	}
	return rows, len(repoIDs), nil
}

// fleetStage names what the run is doing now: the gate it is parked at, else
// the step it is executing. A run that has not started a step yet reports
// nothing rather than naming a step that already finished.
func fleetStage(rv runView) string {
	if gate, ok := rv.awaitingStep(); ok {
		return gate.Name + ":" + gate.Status
	}
	if active := rv.activeRows(); len(active) > 0 {
		return active[0].Step + ":" + active[0].Status
	}
	return ""
}

// fleetActivity reports how recently the run moved: how long it has been parked
// when it is waiting for its driving agent, otherwise the active step's latest
// recorded activity.
func fleetActivity(rv runView) string {
	if rv.AwaitingAgentSince != nil && !terminalStatus(rv.Status) {
		return formatParkedFor(*rv.AwaitingAgentSince)
	}
	if active := rv.activeRows(); len(active) > 0 {
		return active[0].LastActivity
	}
	return ""
}

// fleetChecks reports the run's recorded check outcome. Only persisted
// readiness counts, so a run that has not reached CI reports nothing rather
// than an assumed pass; live CI activity is the stage column's to report.
func fleetChecks(run *db.Run) string {
	if run.CIReadyAt != nil {
		if run.CIReadyNoCI {
			return "no-ci"
		}
		return "passed"
	}
	return ""
}
