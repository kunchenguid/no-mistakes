package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/skill"
	"github.com/spf13/cobra"
)

// recentRunsHomeLimit caps the recent-runs table on the home view. High enough
// to cover normal history in one call, per the AXI minimal-call convention.
const recentRunsHomeLimit = 10

// newAxiCmd builds the agent-facing command tree. Everything under `axi`
// follows AXI conventions: TOON on stdout, progress on stderr, structured
// errors, and explicit exit codes. It is the surface an agent (or the
// /no-mistakes skill) drives; humans use the bare `no-mistakes` TUI instead.
func newAxiCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "axi",
		Short: "Agent interface: drive no-mistakes from an autonomous agent",
		Long: "Agent eXperience Interface for no-mistakes. Prints token-efficient TOON\n" +
			"to stdout and is driven entirely by flags (no interactive prompts).\n" +
			"Running `no-mistakes axi` with no subcommand shows the current state.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-home", "/axi", nil, func() error {
				return runAxiHome(cmd)
			})
		},
	}

	cmd.AddCommand(newAxiRunCmd())
	cmd.AddCommand(newAxiRespondCmd())
	cmd.AddCommand(newAxiStatusCmd())
	cmd.AddCommand(newAxiLogsCmd())
	cmd.AddCommand(newAxiAbortCmd())
	return cmd
}

// axiEnv bundles the resources an axi subcommand needs. Most are DB-backed and
// do not require the daemon; commands that mutate run state ensure it.
type axiEnv struct {
	p      *paths.Paths
	d      *db.DB
	repo   *db.Repo
	client *ipc.Client
}

func (e *axiEnv) close() {
	if e.client != nil {
		e.client.Close()
	}
	if e.d != nil {
		e.d.Close()
	}
}

// openAxiEnv resolves paths, opens the DB, and finds the repo for the current
// directory. When ensureDaemon is true it also starts (if needed) and dials
// the daemon, populating client. Errors are returned for the caller to render
// as structured TOON.
func openAxiEnv(ensureDaemonConn bool) (*axiEnv, error) {
	p, d, err := openResources()
	if err != nil {
		return nil, err
	}
	env := &axiEnv{p: p, d: d}
	repo, err := findRepo(d)
	if err != nil {
		d.Close()
		return nil, err
	}
	env.repo = repo
	if ensureDaemonConn {
		if err := daemon.EnsureDaemon(p); err != nil {
			env.close()
			return nil, fmt.Errorf("start daemon: %w", err)
		}
		client, err := ipc.Dial(p.Socket())
		if err != nil {
			env.close()
			return nil, fmt.Errorf("connect to daemon: %w", err)
		}
		env.client = client
	}
	return env, nil
}

// runAxiHome renders the content-first home view: tool identity, repo, daemon
// state, the active run (if any) with its gate, and recent runs - all from the
// local database so it works whether or not the daemon is running.
func runAxiHome(cmd *cobra.Command) error {
	env, err := openAxiEnv(false)
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	daemonState := "stopped"
	if alive, _ := daemon.IsRunning(env.p); alive {
		daemonState = "running"
	}
	fields := []toon.Field{
		{Key: "bin", Value: collapseHome(executablePath())},
		{Key: "description", Value: skill.Description},
		{Key: "repo", Value: env.repo.WorkingPath},
		{Key: "daemon", Value: daemonState},
	}

	active, err := env.d.GetActiveRun(env.repo.ID, "")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("check active run: %v", err))
	}
	gated := false
	if active != nil {
		steps, _ := env.d.GetStepsByRun(active.ID)
		rv := runViewFromDB(active, steps)
		fields = append(fields, runObjectField(rv))
		if gate, ok := rv.awaitingStep(); ok {
			gated = true
			fields = append(fields, gateFields(gate)...)
		}
	}

	runs, err := env.d.GetRunsByRepo(env.repo.ID)
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("list runs: %v", err))
	}
	fields = append(fields, runsFields(runs, recentRunsHomeLimit)...)

	help := []string{}
	switch {
	case active == nil:
		help = append(help, `Run `+"`"+`no-mistakes axi run --intent "<what the user set out to accomplish>"`+"`"+` to validate your changes`)
	case gated:
		help = append(help, "Run `no-mistakes axi respond --action approve` to clear the current gate")
	default:
		help = append(help, "Run `no-mistakes axi status` to inspect the active run")
	}
	fields = append(fields, toon.Field{Key: "help", Value: help})

	emitDoc(cmd, fields...)
	return nil
}

// runsFields renders a recent-runs table with an aggregate count, showing at
// most limit rows newest-first.
func runsFields(runs []*db.Run, limit int) []toon.Field {
	if len(runs) == 0 {
		return []toon.Field{{Key: "runs", Value: "0 runs yet in this repository"}}
	}
	shown := runs
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	rows := make([]runRow, 0, len(shown))
	for _, r := range shown {
		pr := ""
		if r.PRURL != nil {
			pr = *r.PRURL
		}
		rows = append(rows, runRow{ID: r.ID, Branch: r.Branch, Status: string(r.Status), Head: shortSHA(r.HeadSHA), PR: pr})
	}
	return []toon.Field{
		{Key: "count", Value: fmt.Sprintf("%d of %d total", len(shown), len(runs))},
		{Key: "runs", Value: rows},
	}
}

// repoInitHelp returns an actionable hint when the failure is an uninitialized
// repo, and nothing otherwise.
func repoInitHelp(err error) []string {
	if err != nil && strings.Contains(err.Error(), "not initialized") {
		return []string{"Run `no-mistakes init` to set up the gate in this repository"}
	}
	return nil
}

// executablePath returns the absolute path of the running binary, falling back
// to the invoked name if it cannot be resolved.
func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

// collapseHome rewrites a leading home directory to ~ for compact display.
func collapseHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}
